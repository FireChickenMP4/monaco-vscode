package judge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"exam.local/monaco-examd/internal/store"
)

const refSolution = `package main

import "fmt"

func main() {
	var a, b int
	fmt.Scan(&a, &b)
	fmt.Println(a + b)
}
`

// TestDockerJudgeReferenceSolution 验证 V-07：参考解在 Docker 沙箱里判为通过。
// 没有 Docker 时跳过（不把环境缺失当成失败）。
func TestDockerJudgeReferenceSolution(t *testing.T) {
	ctx := context.Background()
	if err := DockerAvailable(ctx); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	root := t.TempDir()
	ws := filepath.Join(root, "workspaces")
	dir := filepath.Join(ws, "2026001")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(refSolution), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module exam\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	problems, err := LoadProblems(filepath.Join("..", "..", "..", "problems"))
	if err != nil {
		t.Fatal(err)
	}
	j := &Judge{
		Workspaces: ws,
		Problems:   problems,
		Exec:       NewDockerExecutor("exam-go:1.26"),
		TempDir:    filepath.Join(root, "tmp"),
	}
	run := store.JudgeRun{ID: "docker-run", StudentNo: "2026001", Problem: "sum", Status: "running", CreatedAt: time.Now().UTC()}
	res := j.Run(ctx, run)
	if res.Status != "done" || res.Score != res.Total || res.Total == 0 {
		t.Fatalf("reference solution did not pass: status=%s score=%d/%d detail=%s", res.Status, res.Score, res.Total, res.Detail)
	}
}

// TestLocalJudgeReferenceSolution 在宿主上验证同一份参考解，便于无 Docker 的 CI 覆盖判题逻辑。
func TestLocalJudgeReferenceSolution(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspaces")
	dir := filepath.Join(ws, "2026001")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(refSolution), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module exam\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	problems, err := LoadProblems(filepath.Join("..", "..", "..", "problems"))
	if err != nil {
		t.Fatal(err)
	}
	j := &Judge{
		Workspaces: ws,
		Problems:   problems,
		Exec:       &LocalExecutor{},
		TempDir:    filepath.Join(root, "tmp"),
	}
	run := store.JudgeRun{ID: "local-run", StudentNo: "2026001", Problem: "sum", Status: "running", CreatedAt: time.Now().UTC()}
	res := j.Run(context.Background(), run)
	if res.Status != "done" || res.Score != res.Total || res.Total == 0 {
		t.Fatalf("local reference solution did not pass: status=%s score=%d/%d detail=%s", res.Status, res.Score, res.Total, res.Detail)
	}
}
