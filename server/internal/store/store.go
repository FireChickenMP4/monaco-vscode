// Package store 提供 examd 的 SQLite 持久化层。
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound    = errors.New("not found")
	ErrBadPassword = errors.New("invalid token")
	ErrDisabled    = errors.New("student disabled")
	ErrConflict    = errors.New("conflict")
)

// Student 是一名考生账号。
type Student struct {
	No        string    `json:"student_no"`
	Name      string    `json:"name"`
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"created_at"`
}

// Session 是一次浏览器登录会话。
type Session struct {
	ID        string
	StudentNo string
	ExpiresAt time.Time
}

// AuditRecord 是一条写操作审计。
type AuditRecord struct {
	ID        int64     `json:"id"`
	At        time.Time `json:"at"`
	StudentNo string    `json:"student_no"`
	Role      string    `json:"role"`
	Action    string    `json:"action"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	IP        string    `json:"ip"`
	Detail    string    `json:"detail"`
}

// SnapshotMeta 是一次工作区快照的元数据。
type SnapshotMeta struct {
	StudentNo string    `json:"student_no"`
	Version   int       `json:"version"`
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"created_at"`
	Files     int       `json:"files"`
}

// JudgeRun 是一次判题记录。
type JudgeRun struct {
	ID         string    `json:"id"`
	StudentNo  string    `json:"student_no"`
	Problem    string    `json:"problem"`
	Status     string    `json:"status"`
	Score      int       `json:"score"`
	Total      int       `json:"total"`
	Detail     string    `json:"detail"`
	CreatedAt  time.Time `json:"created_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// Store 是 SQLite 之上的一层薄封装。
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）数据库并完成建表。
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS students (
  student_no TEXT PRIMARY KEY,
  name       TEXT NOT NULL DEFAULT '',
  token_hash TEXT NOT NULL,
  disabled   INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT PRIMARY KEY,
  student_no TEXT NOT NULL,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  ip         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sessions_student ON sessions(student_no);
CREATE TABLE IF NOT EXISTS audit (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  at         TEXT NOT NULL,
  student_no TEXT NOT NULL DEFAULT '',
  role       TEXT NOT NULL DEFAULT '',
  action     TEXT NOT NULL,
  path       TEXT NOT NULL DEFAULT '',
  status     INTEGER NOT NULL DEFAULT 0,
  ip         TEXT NOT NULL DEFAULT '',
  detail     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS audit_student ON audit(student_no, id DESC);
CREATE TABLE IF NOT EXISTS snapshots (
  student_no TEXT NOT NULL,
  version    INTEGER NOT NULL,
  hash       TEXT NOT NULL,
  created_at TEXT NOT NULL,
  files      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (student_no, version)
);
CREATE TABLE IF NOT EXISTS judge_runs (
  id          TEXT PRIMARY KEY,
  student_no  TEXT NOT NULL,
  problem     TEXT NOT NULL,
  status      TEXT NOT NULL,
  score       INTEGER NOT NULL DEFAULT 0,
  total       INTEGER NOT NULL DEFAULT 0,
  detail      TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,
  finished_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS judge_student ON judge_runs(student_no, created_at DESC);
`
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

// HashToken 返回令牌的 sha256 十六进制哈希。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewToken 生成 32 字节随机令牌。
func NewToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// NewID 生成 16 字节随机 ID。
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

const timeLayout = time.RFC3339Nano

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// CreateStudent 创建考生账号；token 为空时自动生成。
func (s *Store) CreateStudent(ctx context.Context, no, name, token string) (Student, error) {
	if strings.TrimSpace(no) == "" {
		return Student{}, errors.New("student_no required")
	}
	if token == "" {
		token = NewToken()
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO students(student_no, name, token_hash, disabled, created_at) VALUES(?,?,?,0,?)`,
		no, name, HashToken(token), now.Format(timeLayout))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Student{}, fmt.Errorf("%w: student %s already exists", ErrConflict, no)
		}
		return Student{}, err
	}
	return Student{No: no, Name: name, CreatedAt: now}, nil
}

// SetStudentToken 重置考生令牌。
func (s *Store) SetStudentToken(ctx context.Context, no, token string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE students SET token_hash=? WHERE student_no=?`, HashToken(token), no)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetStudentDisabled 启用/停用考生。
func (s *Store) SetStudentDisabled(ctx context.Context, no string, disabled bool) error {
	v := 0
	if disabled {
		v = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE students SET disabled=? WHERE student_no=?`, v, no)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetStudent 按学号取考生。
func (s *Store) GetStudent(ctx context.Context, no string) (Student, error) {
	row := s.db.QueryRowContext(ctx, `SELECT student_no, name, disabled, created_at FROM students WHERE student_no=?`, no)
	var st Student
	var disabled int
	var created string
	if err := row.Scan(&st.No, &st.Name, &disabled, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Student{}, ErrNotFound
		}
		return Student{}, err
	}
	st.Disabled = disabled != 0
	st.CreatedAt = parseTime(created)
	return st, nil
}

// ListStudents 列出全部考生。
func (s *Store) ListStudents(ctx context.Context) ([]Student, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT student_no, name, disabled, created_at FROM students ORDER BY student_no`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Student
	for rows.Next() {
		var st Student
		var disabled int
		var created string
		if err := rows.Scan(&st.No, &st.Name, &disabled, &created); err != nil {
			return nil, err
		}
		st.Disabled = disabled != 0
		st.CreatedAt = parseTime(created)
		out = append(out, st)
	}
	return out, rows.Err()
}

// Authenticate 校验学号与令牌。
func (s *Store) Authenticate(ctx context.Context, no, token string) (Student, error) {
	row := s.db.QueryRowContext(ctx, `SELECT student_no, name, token_hash, disabled, created_at FROM students WHERE student_no=?`, no)
	var st Student
	var hash, created string
	var disabled int
	if err := row.Scan(&st.No, &st.Name, &hash, &disabled, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Student{}, ErrBadPassword
		}
		return Student{}, err
	}
	if st.Disabled {
		return Student{}, ErrDisabled
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(HashToken(token))) != 1 {
		return Student{}, ErrBadPassword
	}
	st.CreatedAt = parseTime(created)
	return st, nil
}

// CreateSession 建立会话。
func (s *Store) CreateSession(ctx context.Context, studentNo, ip string, ttl time.Duration) (Session, error) {
	id := NewID()
	now := time.Now().UTC()
	exp := now.Add(ttl)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(id, student_no, created_at, expires_at, ip) VALUES(?,?,?,?,?)`,
		id, studentNo, now.Format(timeLayout), exp.Format(timeLayout), ip)
	if err != nil {
		return Session{}, err
	}
	return Session{ID: id, StudentNo: studentNo, ExpiresAt: exp}, nil
}

// SessionStudent 解析会话，过期会话会被删除。
func (s *Store) SessionStudent(ctx context.Context, id string) (string, error) {
	row := s.db.QueryRowContext(ctx, `SELECT student_no, expires_at FROM sessions WHERE id=?`, id)
	var no, exp string
	if err := row.Scan(&no, &exp); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if parseTime(exp).Before(time.Now().UTC()) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
		return "", ErrNotFound
	}
	return no, nil
}

// DeleteSession 注销会话。
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
	return err
}

// PurgeExpiredSessions 清理过期会话，返回删除条数。
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, time.Now().UTC().Format(timeLayout))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Audit 记录一次操作。
func (s *Store) Audit(ctx context.Context, r AuditRecord) {
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO audit(at, student_no, role, action, path, status, ip, detail) VALUES(?,?,?,?,?,?,?,?)`,
		r.At.Format(timeLayout), r.StudentNo, r.Role, r.Action, r.Path, r.Status, r.IP, r.Detail)
}

// ListAudit 按考生（空表示全部）倒序列出审计记录。
func (s *Store) ListAudit(ctx context.Context, studentNo string, limit int) ([]AuditRecord, error) {
	if limit <= 0 || limit > 100000 {
		limit = 200
	}
	var rows *sql.Rows
	var err error
	if studentNo == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, at, student_no, role, action, path, status, ip, detail FROM audit ORDER BY id DESC LIMIT ?`, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, at, student_no, role, action, path, status, ip, detail FROM audit WHERE student_no=? ORDER BY id DESC LIMIT ?`, studentNo, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRecord
	for rows.Next() {
		var r AuditRecord
		var at string
		if err := rows.Scan(&r.ID, &at, &r.StudentNo, &r.Role, &r.Action, &r.Path, &r.Status, &r.IP, &r.Detail); err != nil {
			return nil, err
		}
		r.At = parseTime(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertSnapshot 写入快照元数据。
func (s *Store) InsertSnapshot(ctx context.Context, m SnapshotMeta) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO snapshots(student_no, version, hash, created_at, files) VALUES(?,?,?,?,?)`,
		m.StudentNo, m.Version, m.Hash, m.CreatedAt.Format(timeLayout), m.Files)
	return err
}

// LatestSnapshot 取考生最新快照。
func (s *Store) LatestSnapshot(ctx context.Context, studentNo string) (SnapshotMeta, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT student_no, version, hash, created_at, files FROM snapshots WHERE student_no=? ORDER BY version DESC LIMIT 1`, studentNo)
	var m SnapshotMeta
	var created string
	if err := row.Scan(&m.StudentNo, &m.Version, &m.Hash, &created, &m.Files); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SnapshotMeta{}, ErrNotFound
		}
		return SnapshotMeta{}, err
	}
	m.CreatedAt = parseTime(created)
	return m, nil
}

// ListSnapshots 列出考生快照（倒序）。
func (s *Store) ListSnapshots(ctx context.Context, studentNo string, limit int) ([]SnapshotMeta, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT student_no, version, hash, created_at, files FROM snapshots WHERE student_no=? ORDER BY version DESC LIMIT ?`, studentNo, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotMeta
	for rows.Next() {
		var m SnapshotMeta
		var created string
		if err := rows.Scan(&m.StudentNo, &m.Version, &m.Hash, &created, &m.Files); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetSnapshot 按版本取快照元数据。
func (s *Store) GetSnapshot(ctx context.Context, studentNo string, version int) (SnapshotMeta, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT student_no, version, hash, created_at, files FROM snapshots WHERE student_no=? AND version=?`, studentNo, version)
	var m SnapshotMeta
	var created string
	if err := row.Scan(&m.StudentNo, &m.Version, &m.Hash, &created, &m.Files); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SnapshotMeta{}, ErrNotFound
		}
		return SnapshotMeta{}, err
	}
	m.CreatedAt = parseTime(created)
	return m, nil
}

// InsertJudgeRun 插入判题记录。
func (s *Store) InsertJudgeRun(ctx context.Context, r JudgeRun) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO judge_runs(id, student_no, problem, status, score, total, detail, created_at, finished_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		r.ID, r.StudentNo, r.Problem, r.Status, r.Score, r.Total, r.Detail, r.CreatedAt.Format(timeLayout), formatTime(r.FinishedAt))
	return err
}

// UpdateJudgeRun 更新判题结果。
func (s *Store) UpdateJudgeRun(ctx context.Context, r JudgeRun) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE judge_runs SET status=?, score=?, total=?, detail=?, finished_at=? WHERE id=?`,
		r.Status, r.Score, r.Total, r.Detail, formatTime(r.FinishedAt), r.ID)
	return err
}

// GetJudgeRun 取单条判题记录。
func (s *Store) GetJudgeRun(ctx context.Context, id string) (JudgeRun, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, student_no, problem, status, score, total, detail, created_at, finished_at FROM judge_runs WHERE id=?`, id)
	return scanJudgeRun(row)
}

// ListJudgeRuns 列出判题记录，studentNo 为空表示全部。
func (s *Store) ListJudgeRuns(ctx context.Context, studentNo string, limit int) ([]JudgeRun, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var rows *sql.Rows
	var err error
	if studentNo == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, student_no, problem, status, score, total, detail, created_at, finished_at FROM judge_runs ORDER BY created_at DESC LIMIT ?`, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, student_no, problem, status, score, total, detail, created_at, finished_at FROM judge_runs WHERE student_no=? ORDER BY created_at DESC LIMIT ?`, studentNo, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JudgeRun
	for rows.Next() {
		r, err := scanJudgeRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountJudgeRuns 统计某考生某题的判题次数（用于限流）。
func (s *Store) CountJudgeRuns(ctx context.Context, studentNo, problem string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM judge_runs WHERE student_no=? AND problem=?`, studentNo, problem).Scan(&n)
	return n, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJudgeRun(row rowScanner) (JudgeRun, error) {
	var r JudgeRun
	var created, finished string
	if err := row.Scan(&r.ID, &r.StudentNo, &r.Problem, &r.Status, &r.Score, &r.Total, &r.Detail, &created, &finished); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return JudgeRun{}, ErrNotFound
		}
		return JudgeRun{}, err
	}
	r.CreatedAt = parseTime(created)
	r.FinishedAt = parseTime(finished)
	return r, nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeLayout)
}
