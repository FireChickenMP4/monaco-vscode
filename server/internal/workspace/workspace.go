// Package workspace 实现考生工作区的路径规范化与文件操作。
//
// 所有对外路径都是形如 "/main.go" 的斜杠路径；服务端根目录由调用方给出。
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	ErrForbidden    = errors.New("forbidden path")
	ErrNotFound     = errors.New("not found")
	ErrIsDir        = errors.New("is a directory")
	ErrNotDir       = errors.New("not a directory")
	ErrNotEmpty     = errors.New("directory not empty")
	ErrTooLarge     = errors.New("file too large")
	ErrPrecondition = errors.New("if-match required")
	ErrConflict     = errors.New("conflict")
)

// ConflictError 表示写冲突，Current 是服务端当前的 etag。
type ConflictError struct{ Current string }

func (e *ConflictError) Error() string { return "conflict: stale etag" }
func (e *ConflictError) Unwrap() error { return ErrConflict }

// Entry 是文件或目录的元数据。
type Entry struct {
	Name  string    `json:"name"`
	Path  string    `json:"path"`
	Type  string    `json:"type"`
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
	ETag  string    `json:"etag,omitempty"`
}

// CleanRel 把请求路径规范化为相对根目录的斜杠路径。
// 根目录返回空串；任何 ".."、NUL、盘符或 ADS（冒号）都被拒绝。
func CleanRel(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", ErrForbidden
	}
	p = strings.ReplaceAll(p, "\\", "/")
	segs := strings.Split(p, "/")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		switch s {
		case "", ".":
			continue
		case "..":
			return "", ErrForbidden
		}
		if strings.Contains(s, ":") {
			// Windows 盘符与 NTFS 备用数据流都走这里。
			return "", ErrForbidden
		}
		out = append(out, s)
	}
	return strings.Join(out, "/"), nil
}

// Resolve 把对外路径解析为绝对路径，并校验不能逃逸出 root。
func Resolve(root, p string) (abs string, rel string, err error) {
	rel, err = CleanRel(p)
	if err != nil {
		return "", "", err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	if rel == "" {
		return root, "", nil
	}
	abs = filepath.Join(root, filepath.FromSlash(rel))
	relToRoot, err := filepath.Rel(root, abs)
	if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", "", ErrForbidden
	}
	if err := checkNoSymlink(root, rel); err != nil {
		return "", "", err
	}
	return abs, rel, nil
}

// checkNoSymlink 拒绝路径中任何已存在的符号链接，防止借链接逃逸。
func checkNoSymlink(root, rel string) error {
	cur := root
	for _, seg := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return ErrForbidden
		}
	}
	return nil
}

func apiPath(rel string) string { return "/" + rel }

// quoteETag 生成强 etag。
func quoteETag(sum []byte) string { return `"` + hex.EncodeToString(sum) + `"` }

// ETagBytes 计算内容的 etag。
func ETagBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return quoteETag(sum[:])
}

func statEntry(abs, rel string) (Entry, error) {
	fi, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return Entry{}, ErrNotFound
		}
		return Entry{}, err
	}
	e := Entry{
		Name:  filepath.Base(abs),
		Path:  apiPath(rel),
		Mtime: fi.ModTime().UTC(),
	}
	if rel == "" {
		e.Name = "/"
	}
	if fi.IsDir() {
		e.Type = "dir"
		return e, nil
	}
	e.Type = "file"
	e.Size = fi.Size()
	data, err := os.ReadFile(abs)
	if err != nil {
		return Entry{}, err
	}
	e.ETag = ETagBytes(data)
	return e, nil
}

// Stat 返回路径元数据。
func Stat(root, p string) (Entry, error) {
	abs, rel, err := Resolve(root, p)
	if err != nil {
		return Entry{}, err
	}
	return statEntry(abs, rel)
}

// List 列出目录内容（目录在前，同类按名字排序）。
func List(root, p string) ([]Entry, error) {
	abs, rel, err := Resolve(root, p)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !fi.IsDir() {
		return nil, ErrNotDir
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		name := de.Name()
		if strings.HasSuffix(name, ".tmp-examd") {
			continue
		}
		childRel := name
		if rel != "" {
			childRel = rel + "/" + name
		}
		e := Entry{Name: name, Path: apiPath(childRel)}
		info, err := de.Info()
		if err == nil {
			e.Mtime = info.ModTime().UTC()
		}
		if de.IsDir() {
			e.Type = "dir"
		} else {
			e.Type = "file"
			if info != nil {
				e.Size = info.Size()
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type == "dir"
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Read 读取文件内容。
func Read(root, p string, maxSize int64) ([]byte, Entry, error) {
	abs, rel, err := Resolve(root, p)
	if err != nil {
		return nil, Entry{}, err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, Entry{}, ErrNotFound
		}
		return nil, Entry{}, err
	}
	if fi.IsDir() {
		return nil, Entry{}, ErrIsDir
	}
	if maxSize > 0 && fi.Size() > maxSize {
		return nil, Entry{}, ErrTooLarge
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, Entry{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, Entry{}, err
	}
	if maxSize > 0 && int64(len(data)) > maxSize {
		return nil, Entry{}, ErrTooLarge
	}
	e := Entry{
		Name:  filepath.Base(abs),
		Path:  apiPath(rel),
		Type:  "file",
		Size:  int64(len(data)),
		Mtime: fi.ModTime().UTC(),
		ETag:  ETagBytes(data),
	}
	return data, e, nil
}

// Write 写入文件，按 If-Match 语义做乐观并发控制。
//
// 文件已存在时 If-Match 必填（"*" 表示无条件覆盖）；文件不存在时只接受空或 "*"。
func Write(root, p string, data []byte, ifMatch string, maxSize int64) (Entry, error) {
	if maxSize > 0 && int64(len(data)) > maxSize {
		return Entry{}, ErrTooLarge
	}
	abs, rel, err := Resolve(root, p)
	if err != nil {
		return Entry{}, err
	}
	if rel == "" {
		return Entry{}, ErrIsDir
	}
	fi, statErr := os.Lstat(abs)
	exists := statErr == nil
	if exists {
		if fi.IsDir() {
			return Entry{}, ErrIsDir
		}
		cur, err := os.ReadFile(abs)
		if err != nil {
			return Entry{}, err
		}
		curTag := ETagBytes(cur)
		switch {
		case ifMatch == "":
			return Entry{}, ErrPrecondition
		case ifMatch != "*" && ifMatch != curTag:
			return Entry{}, &ConflictError{Current: curTag}
		}
	} else if ifMatch != "" && ifMatch != "*" {
		return Entry{}, &ConflictError{Current: ""}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return Entry{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".write-*.tmp-examd")
	if err != nil {
		return Entry{}, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return Entry{}, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return Entry{}, err
	}
	// Windows 上快照进程可能正在读同一个文件，rename 会短暂失败（sharing violation）。
	// 重试几次，仍失败则退化为原地写。
	var renameErr error
	for attempt := 0; attempt < 5; attempt++ {
		if err := os.Rename(tmpName, abs); err == nil {
			renameErr = nil
			break
		} else {
			renameErr = err
			time.Sleep(time.Duration(10*(attempt+1)) * time.Millisecond)
		}
	}
	if renameErr != nil {
		os.Remove(tmpName)
		if err := os.WriteFile(abs, data, 0o644); err != nil {
			return Entry{}, renameErr
		}
	}
	return statEntry(abs, rel)
}

// Mkdir 创建目录（等价 mkdir -p，已存在不报错）。
func Mkdir(root, p string) (Entry, error) {
	abs, rel, err := Resolve(root, p)
	if err != nil {
		return Entry{}, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return Entry{}, err
	}
	return statEntry(abs, rel)
}

// Rename 重命名；overwrite 为 false 时目标存在返回 ErrConflict。
func Rename(root, from, to string, overwrite bool) (Entry, error) {
	srcAbs, _, err := Resolve(root, from)
	if err != nil {
		return Entry{}, err
	}
	dstAbs, dstRel, err := Resolve(root, to)
	if err != nil {
		return Entry{}, err
	}
	if dstRel == "" {
		return Entry{}, ErrForbidden
	}
	if _, err := os.Lstat(srcAbs); err != nil {
		if os.IsNotExist(err) {
			return Entry{}, ErrNotFound
		}
		return Entry{}, err
	}
	if _, err := os.Lstat(dstAbs); err == nil && !overwrite {
		return Entry{}, ErrConflict
	}
	if err := os.MkdirAll(filepath.Dir(dstAbs), 0o755); err != nil {
		return Entry{}, err
	}
	if err := os.Rename(srcAbs, dstAbs); err != nil {
		return Entry{}, err
	}
	return statEntry(dstAbs, dstRel)
}

// Remove 删除文件或目录；目录非空且未指定 recursive 时返回 ErrNotEmpty。
func Remove(root, p string, recursive bool) error {
	abs, rel, err := Resolve(root, p)
	if err != nil {
		return err
	}
	if rel == "" {
		return ErrForbidden
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	if fi.IsDir() {
		if !recursive {
			des, err := os.ReadDir(abs)
			if err != nil {
				return err
			}
			if len(des) > 0 {
				return ErrNotEmpty
			}
		}
		return os.RemoveAll(abs)
	}
	return os.Remove(abs)
}

// TreeHash 计算工作区内容哈希，跳过临时文件与符号链接。
func TreeHash(root string) (hash string, files int, err error) {
	type item struct {
		rel string
		sum string
	}
	var items []item
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if p == root {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".tmp-examd") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		items = append(items, item{rel: rel, sum: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })
	h := sha256.New()
	for _, it := range items {
		fmt.Fprintf(h, "%s\x00%s\n", it.rel, it.sum)
	}
	return hex.EncodeToString(h.Sum(nil)), len(items), nil
}
