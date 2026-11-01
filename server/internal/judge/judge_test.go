package judge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"exam.local/monaco-examd/internal/store"
)

// fakeExec 按 A+B 题目语义返回结果，用于在没有 Docker 时测试判题流程。
type fakeExec struct {
	buildLog string
	buildErr error
	out      string
}

func (f *fakeExec) Build(ctx context.Context, srcDir, outDir string) (string, error) {
	return f.buildLog, f.buildErr
}

func (f *fakeExec) Run(ctx context.Context, outDir, stdin string, opt RunOptions) (string, string, error) {
	if f.out != "" {
		return f.out, "", nil
	}
	fields := strings.Fields(stdin)
	if len(fields) != 2 {
		return "", "bad input", nil
	}
	a, _ := strconv.Atoi(fields[0])
	b, _ := strconv.Atoi(fields[1])
	return fmt.Sprintf("%d\n", a+b), "", nil
}

func setupJudge(t *testing.T) (*Judge, store.JudgeRun, string) {
	t.Helper()
	root := t.TempDir()
	ws := filepath.Join(root, "workspaces")
	dir := filepath.Join(ws, "2026001")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module exam\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	problems, err := LoadProblems(filepath.Join("..", "..", "..", "problems"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := problems["sum"]; !ok {
		t.Fatalf("sum problem not loaded: %v", problems)
	}
	j := &Judge{Workspaces: ws, Problems: problems, TempDir: filepath.Join(root, "tmp")}
	run := store.JudgeRun{ID: "run-1", StudentNo: "2026001", Problem: "sum", Status: "running", CreatedAt: time.Now().UTC()}
	return j, run, root
}

func TestJudgeAcceptsCorrectOutput(t *testing.T) {
	j, run, _ := setupJudge(t)
	j.Exec = &fakeExec{}
	res := j.Run(context.Background(), run)
	if res.Status != "done" || res.Score != 3 || res.Total != 3 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Detail, "accepted") {
		t.Fatalf("detail = %q", res.Detail)
	}
}

func TestJudgeReportsWrongAnswer(t *testing.T) {
	j, run, _ := setupJudge(t)
	j.Exec = &fakeExec{out: "999\n"}
	res := j.Run(context.Background(), run)
	if res.Score != 0 || res.Total != 3 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Detail, "wrong answer") {
		t.Fatalf("detail = %q", res.Detail)
	}
}

func TestJudgeReportsCompileFailure(t *testing.T) {
	j, run, _ := setupJudge(t)
	j.Exec = &fakeExec{buildErr: fmt.Errorf("exit 1"), buildLog: "./main.go:1: syntax error"}
	res := j.Run(context.Background(), run)
	if res.Status != "failed" || !strings.Contains(res.Detail, "compile failed") {
		t.Fatalf("result = %+v", res)
	}
}

func TestJudgeUnknownProblem(t *testing.T) {
	j, run, _ := setupJudge(t)
	run.Problem = "nope"
	res := j.Run(context.Background(), run)
	if res.Status != "failed" || !strings.Contains(res.Detail, "unknown problem") {
		t.Fatalf("result = %+v", res)
	}
}
