// Package snapshot 按固定间隔为每个考生工作区做快照。
package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"exam.local/monaco-examd/internal/store"
	"exam.local/monaco-examd/internal/workspace"
)

// Result 描述一次快照结果。
type Result struct {
	StudentNo string `json:"student_no"`
	Version   int    `json:"version"`
	Hash      string `json:"hash"`
	Files     int    `json:"files"`
	Changed   bool   `json:"changed"`
}

// Manager 周期性扫描工作区，内容变化时落一份快照。
type Manager struct {
	Workspaces string
	Snapshots  string
	Store      *store.Store
	Interval   time.Duration
	Log        *slog.Logger

	mu   sync.Mutex
	last map[string]string
}

// New 创建快照管理器。
func New(workspaces, snapshots string, st *store.Store, interval time.Duration, log *slog.Logger) *Manager {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		Workspaces: workspaces,
		Snapshots:  snapshots,
		Store:      st,
		Interval:   interval,
		Log:        log,
		last:       map[string]string{},
	}
}

// Init 从数据库恢复每个考生最近一次快照的哈希，避免重启后重复快照。
func (m *Manager) Init(ctx context.Context) error {
	students, err := m.Store.ListStudents(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range students {
		latest, err := m.Store.LatestSnapshot(ctx, s.No)
		if err != nil {
			continue
		}
		m.last[s.No] = latest.Hash
	}
	return nil
}

// Run 阻塞运行，直到 ctx 取消。
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(m.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := m.SnapshotAll(ctx); err != nil {
				m.Log.Warn("snapshot round failed", "err", err)
			}
		}
	}
}

// SnapshotAll 对所有工作区做一次检查。
func (m *Manager) SnapshotAll(ctx context.Context) ([]Result, error) {
	des, err := os.ReadDir(m.Workspaces)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Result
	var firstErr error
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		res, err := m.SnapshotOne(ctx, de.Name())
		if err != nil {
			m.Log.Warn("snapshot failed", "student", de.Name(), "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, res)
	}
	return out, firstErr
}

// SnapshotOne 为单个考生生成快照（内容未变时 Changed=false）。
func (m *Manager) SnapshotOne(ctx context.Context, studentNo string) (Result, error) {
	root := filepath.Join(m.Workspaces, studentNo)
	if _, err := os.Stat(root); err != nil {
		return Result{}, err
	}
	hash, files, err := workspace.TreeHash(root)
	if err != nil {
		return Result{}, err
	}
	res := Result{StudentNo: studentNo, Hash: hash, Files: files}
	m.mu.Lock()
	lastHash, seen := m.last[studentNo]
	m.mu.Unlock()
	if seen && lastHash == hash {
		if latest, err := m.Store.LatestSnapshot(ctx, studentNo); err == nil {
			res.Version = latest.Version
		}
		return res, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	version := 1
	if latest, err := m.Store.LatestSnapshot(ctx, studentNo); err == nil {
		version = latest.Version + 1
	}
	dir := filepath.Join(m.Snapshots, studentNo, fmt.Sprintf("v%06d", version))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	if err := copyTree(root, dir); err != nil {
		return Result{}, err
	}
	meta := map[string]any{
		"student_no": studentNo,
		"version":    version,
		"hash":       hash,
		"files":      files,
		"created_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), mb, 0o644); err != nil {
		return Result{}, err
	}
	if err := m.Store.InsertSnapshot(ctx, store.SnapshotMeta{
		StudentNo: studentNo,
		Version:   version,
		Hash:      hash,
		CreatedAt: time.Now().UTC(),
		Files:     files,
	}); err != nil {
		return Result{}, err
	}
	m.last[studentNo] = hash
	res.Version = version
	res.Changed = true
	m.Log.Info("snapshot written", "student", studentNo, "version", version, "files", files)
	return res, nil
}

// Restore 把某个快照恢复到工作区（先清空工作区）。
func (m *Manager) Restore(ctx context.Context, studentNo string, version int) error {
	src := filepath.Join(m.Snapshots, studentNo, fmt.Sprintf("v%06d", version))
	if _, err := os.Stat(src); err != nil {
		return err
	}
	dst := filepath.Join(m.Workspaces, studentNo)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if err := copyTree(src, dst); err != nil {
		return err
	}
	// meta.json 只是快照附带信息，不写回工作区。
	_ = os.Remove(filepath.Join(dst, "meta.json"))
	m.mu.Lock()
	delete(m.last, studentNo)
	m.mu.Unlock()
	return nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".tmp-examd") {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
