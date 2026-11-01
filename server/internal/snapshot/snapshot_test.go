package snapshot

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"exam.local/monaco-examd/internal/store"
)

func TestSnapshotLifecycle(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspaces")
	snaps := filepath.Join(root, "snapshots")
	student := "2026001"
	dir := filepath.Join(ws, student)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(root, "examd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateStudent(context.Background(), student, "张三", "tok"); err != nil {
		t.Fatal(err)
	}

	m := New(ws, snaps, st, 15*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	res, err := m.SnapshotOne(context.Background(), student)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Version != 1 || res.Files != 1 {
		t.Fatalf("first snapshot = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(snaps, student, "v000001", "main.go")); err != nil {
		t.Fatalf("snapshot content missing: %v", err)
	}

	res, err = m.SnapshotOne(context.Background(), student)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || res.Version != 1 {
		t.Fatalf("unchanged snapshot = %+v", res)
	}

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = m.SnapshotOne(context.Background(), student)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Version != 2 {
		t.Fatalf("second snapshot = %+v", res)
	}

	list, err := st.ListSnapshots(context.Background(), student, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Version != 2 {
		t.Fatalf("db snapshots = %+v", list)
	}

	// 回放 v1。
	if err := m.Restore(context.Background(), student, 1); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil || string(b) != "v1" {
		t.Fatalf("restore = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err == nil {
		t.Fatal("meta.json should not be restored into the workspace")
	}
}
