// Package judge 在服务端沙箱里编译并运行考生工作区。
package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"exam.local/monaco-examd/internal/store"
)

// Case 是一组输入/期望输出。
type Case struct {
	Input  string `json:"input"`
	Output string `json:"output"`
}

// Problem 是一道题的定义（problems/<id>.json）。
type Problem struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	TimeLimitMS int    `json:"time_limit_ms"`
	MemoryMB    int    `json:"memory_mb"`
	Cases       []Case `json:"cases"`
}

// LoadProblems 读取目录下全部 *.json 题目。
func LoadProblems(dir string) (map[string]Problem, error) {
	out := map[string]Problem{}
	if dir == "" {
		return out, nil
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			return nil, err
		}
		var p Problem
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("%s: %w", de.Name(), err)
		}
		if p.ID == "" {
			p.ID = strings.TrimSuffix(de.Name(), ".json")
		}
		if p.TimeLimitMS <= 0 {
			p.TimeLimitMS = 10000
		}
		if p.MemoryMB <= 0 {
			p.MemoryMB = 512
		}
		out[p.ID] = p
	}
	return out, nil
}

// RunOptions 是单次运行的资源约束。
type RunOptions struct {
	MemoryMB int
	Timeout  time.Duration
}

// Executor 抽象编译/运行环境，便于在没有 Docker 时用本地实现测试。
type Executor interface {
	Build(ctx context.Context, srcDir, outDir string) (string, error)
	Run(ctx context.Context, outDir, stdin string, opt RunOptions) (stdout, stderr string, err error)
}

// DockerExecutor 用 Docker 容器编译和运行。
type DockerExecutor struct {
	Image string
}

// NewDockerExecutor 返回使用指定镜像的执行器。
func NewDockerExecutor(image string) *DockerExecutor {
	if image == "" {
		image = "exam-go:1.26"
	}
	return &DockerExecutor{Image: image}
}

func (d *DockerExecutor) dockerCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	return cmd
}

func (d *DockerExecutor) Build(ctx context.Context, srcDir, outDir string) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	cmd := d.dockerCmd(ctx, "run", "--rm", "--network=none",
		"--memory=1g", "--cpus=2", "--pids-limit=256",
		"-v", srcDir+":/src:ro", "-v", outDir+":/out",
		"-w", "/src", "-e", "CGO_ENABLED=0", "-e", "GOFLAGS=-mod=mod",
		d.Image, "sh", "-c", "go build -trimpath -buildvcs=false -o /out/prog .")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func (d *DockerExecutor) Run(ctx context.Context, outDir, stdin string, opt RunOptions) (string, string, error) {
	if opt.MemoryMB <= 0 {
		opt.MemoryMB = 512
	}
	args := []string{"run", "--rm", "-i", "--network=none",
		fmt.Sprintf("--memory=%dm", opt.MemoryMB), "--cpus=1", "--pids-limit=128",
		"-v", outDir + ":/out:ro", d.Image, "/out/prog"}
	cmd := d.dockerCmd(ctx, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// LocalExecutor 直接在本机执行，仅供测试和开发使用。
type LocalExecutor struct {
	GoBin string
}

// Build 在宿主上编译。
func (l *LocalExecutor) Build(ctx context.Context, srcDir, outDir string) (string, error) {
	bin := l.GoBin
	if bin == "" {
		bin = "go"
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	name := "prog"
	if filepath.Separator == '\\' {
		name = "prog.exe"
	}
	cmd := exec.CommandContext(ctx, bin, "build", "-trimpath", "-buildvcs=false", "-o", filepath.Join(outDir, name), ".")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=-mod=mod")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// Run 在宿主上运行。
func (l *LocalExecutor) Run(ctx context.Context, outDir, stdin string, opt RunOptions) (string, string, error) {
	name := "prog"
	if filepath.Separator == '\\' {
		name = "prog.exe"
	}
	cmd := exec.CommandContext(ctx, filepath.Join(outDir, name))
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// Judge 负责准备沙箱、编译、运行和评分。
type Judge struct {
	Workspaces string
	Problems   map[string]Problem
	Exec       Executor
	TempDir    string
}

// Run 对一次判题记录执行全部用例，返回更新后的记录。
func (j *Judge) Run(ctx context.Context, run store.JudgeRun) store.JudgeRun {
	run.FinishedAt = time.Now().UTC()
	p, ok := j.Problems[run.Problem]
	if !ok {
		run.Status = "failed"
		run.Detail = fmt.Sprintf("unknown problem %q", run.Problem)
		return run
	}
	if j.Exec == nil {
		run.Status = "failed"
		run.Detail = "no executor configured (docker unavailable)"
		return run
	}
	ws := filepath.Join(j.Workspaces, run.StudentNo)
	if _, err := os.Stat(ws); err != nil {
		run.Status = "failed"
		run.Detail = fmt.Sprintf("workspace missing: %v", err)
		return run
	}
	base := j.TempDir
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		run.Status = "failed"
		run.Detail = err.Error()
		return run
	}
	tmp, err := os.MkdirTemp(base, "judge-*")
	if err != nil {
		run.Status = "failed"
		run.Detail = err.Error()
		return run
	}
	defer os.RemoveAll(tmp)
	src := filepath.Join(tmp, "src")
	out := filepath.Join(tmp, "out")
	if err := os.MkdirAll(src, 0o755); err != nil {
		run.Status = "failed"
		run.Detail = err.Error()
		return run
	}
	if err := copyDir(ws, src); err != nil {
		run.Status = "failed"
		run.Detail = fmt.Sprintf("copy workspace: %v", err)
		return run
	}
	if _, err := os.Stat(filepath.Join(src, "go.mod")); err != nil {
		if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module exam\n\ngo 1.26\n"), 0o644); err != nil {
			run.Status = "failed"
			run.Detail = err.Error()
			return run
		}
	}
	run.Total = len(p.Cases)

	buildCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	buildLog, err := j.Exec.Build(buildCtx, src, out)
	cancel()
	if err != nil {
		run.Status = "failed"
		run.Detail = "compile failed\n" + truncate(buildLog, 4000)
		return run
	}

	var failures []string
	for i, c := range p.Cases {
		runCtx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeLimitMS)*time.Millisecond)
		stdout, stderr, err := j.Exec.Run(runCtx, out, c.Input, RunOptions{MemoryMB: p.MemoryMB, Timeout: time.Duration(p.TimeLimitMS) * time.Millisecond})
		timeout := errors.Is(runCtx.Err(), context.DeadlineExceeded)
		cancel()
		switch {
		case timeout:
			failures = append(failures, fmt.Sprintf("case %d: time limit exceeded", i+1))
		case err != nil:
			failures = append(failures, fmt.Sprintf("case %d: runtime error: %v %s", i+1, err, truncate(stderr, 400)))
		case normalize(stdout) != normalize(c.Output):
			failures = append(failures, fmt.Sprintf("case %d: wrong answer", i+1))
		default:
			run.Score++
		}
	}
	run.FinishedAt = time.Now().UTC()
	if len(failures) > 0 {
		run.Status = "done"
		run.Detail = strings.Join(failures, "\n")
		return run
	}
	run.Status = "done"
	run.Detail = fmt.Sprintf("accepted: %d/%d", run.Score, run.Total)
	return run
}

func normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func copyDir(src, dst string) error {
	var items []string
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		items = append(items, p)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(items)
	for _, p := range items {
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			continue
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		outf, err := os.Create(target)
		if err != nil {
			in.Close()
			return err
		}
		if _, err := io.Copy(outf, in); err != nil {
			in.Close()
			outf.Close()
			return err
		}
		in.Close()
		if err := outf.Close(); err != nil {
			return err
		}
	}
	return nil
}

// DockerAvailable 检查 Docker 守护进程是否可用。
func DockerAvailable(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	return cmd.Run()
}
