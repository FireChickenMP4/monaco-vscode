package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCleanRelRejectsTraversal(t *testing.T) {
	bad := []string{
		"../etc/passwd",
		"a/../../b",
		"..",
		"a\\..\\b",
		"/../x",
		"file.txt:stream",
		"C:/windows",
		"a\x00b",
	}
	for _, p := range bad {
		if rel, err := CleanRel(p); err == nil {
			t.Fatalf("CleanRel(%q) = %q, want error", p, rel)
		}
	}
	ok := map[string]string{
		"/":            "",
		"":             "",
		"/main.go":     "main.go",
		"dir/sub/file": "dir/sub/file",
	}
	for in, want := range ok {
		if want == "" && strings.Contains(in, "..") {
			continue
		}
		got, err := CleanRel(in)
		if err != nil || got != want {
			t.Fatalf("CleanRel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestResolveStaysInsideRoot(t *testing.T) {
	root := newRoot(t)
	abs, rel, err := Resolve(root, "/sub/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if rel != "sub/a.txt" {
		t.Fatalf("rel = %q", rel)
	}
	if !strings.HasPrefix(abs, root) {
		t.Fatalf("abs %q escapes root %q", abs, root)
	}
	if _, _, err := Resolve(root, "../../secret"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

func TestWriteRequiresMatchingETag(t *testing.T) {
	root := newRoot(t)
	first, err := Write(root, "/main.go", []byte("package main\n"), "*", 1<<20)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if first.ETag == "" {
		t.Fatal("empty etag")
	}
	// 不带 If-Match：必须拒绝。
	if _, err := Write(root, "/main.go", []byte("x"), "", 1<<20); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("want ErrPrecondition, got %v", err)
	}
	// 过期 etag：冲突，并带回当前 etag。
	_, err = Write(root, "/main.go", []byte("x"), `"stale"`, 1<<20)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Current != first.ETag {
		t.Fatalf("want conflict with current etag, got %v", err)
	}
	// 正确 etag：写入成功且 etag 变化。
	second, err := Write(root, "/main.go", []byte("package main\n// v2\n"), first.ETag, 1<<20)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if second.ETag == first.ETag {
		t.Fatal("etag did not change")
	}
	data, _, err := Read(root, "/main.go", 1<<20)
	if err != nil || string(data) != "package main\n// v2\n" {
		t.Fatalf("read = %q, %v", data, err)
	}
}

func TestWriteRejectsOversize(t *testing.T) {
	root := newRoot(t)
	if _, err := Write(root, "/big.go", make([]byte, 32), "*", 16); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestMkdirListRenameDelete(t *testing.T) {
	root := newRoot(t)
	if _, err := Mkdir(root, "/pkg/deep"); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(root, "/pkg/deep/a.go", []byte("a"), "*", 1<<20); err != nil {
		t.Fatal(err)
	}
	entries, err := List(root, "/pkg")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "deep" || entries[0].Type != "dir" {
		t.Fatalf("entries = %+v", entries)
	}
	if _, err := Rename(root, "/pkg/deep/a.go", "/pkg/a.go", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read(root, "/pkg/a.go", 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := Remove(root, "/pkg", false); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("want ErrNotEmpty, got %v", err)
	}
	if err := Remove(root, "/pkg", true); err != nil {
		t.Fatal(err)
	}
	if _, err := Stat(root, "/pkg"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestTreeHashDetectsChanges(t *testing.T) {
	root := newRoot(t)
	if _, err := Write(root, "/a.go", []byte("a"), "*", 1<<20); err != nil {
		t.Fatal(err)
	}
	h1, n1, err := TreeHash(root)
	if err != nil {
		t.Fatal(err)
	}
	if n1 != 1 {
		t.Fatalf("files = %d", n1)
	}
	h2, _, err := TreeHash(root)
	if err != nil || h1 != h2 {
		t.Fatalf("hash not stable: %v", err)
	}
	if _, err := Write(root, "/a.go", []byte("b"), "*", 1<<20); err != nil {
		t.Fatal(err)
	}
	h3, _, err := TreeHash(root)
	if err != nil || h3 == h1 {
		t.Fatalf("hash did not change: %v", err)
	}
}
