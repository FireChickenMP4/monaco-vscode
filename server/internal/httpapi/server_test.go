package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exam.local/monaco-examd/internal/judge"
	"exam.local/monaco-examd/internal/snapshot"
	"exam.local/monaco-examd/internal/store"
	"exam.local/monaco-examd/internal/workspace"
)

const testAdminToken = "test-admin-token"

type testEnv struct {
	t         *testing.T
	server    *httptest.Server
	store     *store.Store
	snap      *snapshot.Manager
	data      string
	workspace string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	ws := filepath.Join(root, "workspaces")
	snaps := filepath.Join(root, "snapshots")
	web := filepath.Join(root, "web")
	for _, d := range []string{ws, snaps, web} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<html>workbench</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(root, "examd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	snap := snapshot.New(ws, snaps, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	api := New(Config{
		WebDir:       web,
		WorkspaceDir: ws,
		SessionTTL:   time.Hour,
		MaxFileSize:  1 << 20,
		AdminToken:   testAdminToken,
		JudgeLimit:   20,
	}, st, snap, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return &testEnv{t: t, server: srv, store: st, snap: snap, data: root, workspace: ws}
}

func (e *testEnv) admin(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Timeout: 10 * time.Second}
}

func (e *testEnv) adminDo(method, path string, body any) *http.Response {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.server.URL+path, rdr)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := e.admin(e.t).Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func (e *testEnv) createStudent(no, name, token string) {
	e.t.Helper()
	res := e.adminDo(http.MethodPost, "/api/admin/students", map[string]any{
		"student_no": no, "name": name, "token": token,
	})
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(res.Body)
		e.t.Fatalf("create student: %d %s", res.StatusCode, b)
	}
}

func (e *testEnv) login(no, token string) *http.Client {
	e.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		e.t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	b, _ := json.Marshal(map[string]string{"student_no": no, "token": token})
	res, err := client.Post(e.server.URL+"/api/login", "application/json", bytes.NewReader(b))
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		e.t.Fatalf("login %s: %d %s", no, res.StatusCode, body)
	}
	return client
}

func doJSON(t *testing.T, client *http.Client, method, url string, body any, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(res.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return res, out
}

func TestLoginRequiresValidToken(t *testing.T) {
	e := newTestEnv(t)
	e.createStudent("2026001", "张三", "tok-1")

	anon := &http.Client{}
	res, body := doJSON(t, anon, http.MethodGet, e.server.URL+"/api/session", nil, nil)
	if res.StatusCode != http.StatusOK || body["authenticated"] != false {
		t.Fatalf("anonymous session = %d %v", res.StatusCode, body)
	}

	res, _ = doJSON(t, anon, http.MethodPost, e.server.URL+"/api/login", map[string]string{
		"student_no": "2026001", "token": "wrong",
	}, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}

	client := e.login("2026001", "tok-1")

	res, body = doJSON(t, client, http.MethodGet, e.server.URL+"/api/session", nil, nil)
	if res.StatusCode != http.StatusOK || body["authenticated"] != true || body["student_no"] != "2026001" {
		t.Fatalf("authenticated session = %d %v", res.StatusCode, body)
	}

	res, _ = doJSON(t, anon, http.MethodGet, e.server.URL+"/api/me", nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/api/me without session = %d, want 401", res.StatusCode)
	}
}

func TestFileRoundTripAndETag(t *testing.T) {
	e := newTestEnv(t)
	e.createStudent("2026001", "张三", "tok-1")
	client := e.login("2026001", "tok-1")
	base := e.server.URL

	// 工作区已初始化为模板。
	res, body := doJSON(t, client, http.MethodGet, base+"/api/fs/list?path=/", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %v", res.StatusCode, body)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) == 0 {
		t.Fatal("empty workspace")
	}

	// 读 main.go 拿 etag。
	res, _ = doJSON(t, client, http.MethodGet, base+"/api/fs/stat?path=/main.go", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stat: %d", res.StatusCode)
	}
	etag := res.Header.Get("ETag")
	if etag == "" {
		t.Fatal("missing etag")
	}

	content := "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(42) }\n"
	req, _ := http.NewRequest(http.MethodPut, base+"/api/fs/write?path=/main.go", strings.NewReader(content))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("If-Match", etag)
	res2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	bodyBytes, _ := io.ReadAll(res2.Body)
	res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("write: %d %s", res2.StatusCode, bodyBytes)
	}

	// 重新读取内容必须一致（V-02）。
	got, err := client.Get(base + "/api/fs/read?path=/main.go")
	if err != nil {
		t.Fatal(err)
	}
	gotBody, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if string(gotBody) != content {
		t.Fatalf("read back = %q", gotBody)
	}
	if got.Header.Get("ETag") == etag {
		t.Fatal("etag unchanged after write")
	}
}

func TestWriteConflictsReturn409(t *testing.T) {
	e := newTestEnv(t)
	e.createStudent("2026001", "张三", "tok-1")
	client := e.login("2026001", "tok-1")
	base := e.server.URL

	res, _ := doJSON(t, client, http.MethodGet, base+"/api/fs/stat?path=/main.go", nil, nil)
	etag := res.Header.Get("ETag")

	// 模拟另一个标签页用旧 etag 抢先写入。
	req, _ := http.NewRequest(http.MethodPut, base+"/api/fs/write?path=/main.go", strings.NewReader("v1"))
	req.Header.Set("If-Match", etag)
	if r, err := client.Do(req); err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("first write: %v %v", err, r)
	}

	// 旧 etag 再写一次 -> 409 且带当前 etag。
	req, _ = http.NewRequest(http.MethodPut, base+"/api/fs/write?path=/main.go", strings.NewReader("v2"))
	req.Header.Set("If-Match", etag)
	res2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res2.StatusCode)
	}
	var payload map[string]any
	_ = json.NewDecoder(res2.Body).Decode(&payload)
	if payload["current_etag"] == "" {
		t.Fatalf("missing current_etag: %v", payload)
	}

	// 缺 If-Match -> 428。
	req, _ = http.NewRequest(http.MethodPut, base+"/api/fs/write?path=/main.go", strings.NewReader("v3"))
	res3, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("status = %d, want 428", res3.StatusCode)
	}
}

func TestPathTraversalForbidden(t *testing.T) {
	e := newTestEnv(t)
	e.createStudent("2026001", "张三", "tok-1")
	client := e.login("2026001", "tok-1")
	base := e.server.URL

	for _, p := range []string{
		"/api/fs/read?path=../../examd.db",
		"/api/fs/read?path=..%2F..%2Fexamd.db",
		"/api/fs/read?path=/..%2F..%2Fexamd.db",
		"/api/fs/list?path=../../",
	} {
		res, _ := doJSON(t, client, http.MethodGet, base+p, nil, nil)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", p, res.StatusCode)
		}
	}
}

func TestStudentsAreIsolated(t *testing.T) {
	e := newTestEnv(t)
	e.createStudent("2026001", "张三", "tok-1")
	e.createStudent("2026002", "李四", "tok-2")
	a := e.login("2026001", "tok-1")
	b := e.login("2026002", "tok-2")

	req, _ := http.NewRequest(http.MethodPut, e.server.URL+"/api/fs/write?path=/secret.txt", strings.NewReader("A secret"))
	req.Header.Set("If-Match", "*")
	if r, err := a.Do(req); err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("write: %v %v", err, r)
	}
	res, _ := doJSON(t, b, http.MethodGet, e.server.URL+"/api/fs/read?path=/secret.txt", nil, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("student B read A's file: %d", res.StatusCode)
	}
}

func TestSnapshotAndAudit(t *testing.T) {
	e := newTestEnv(t)
	e.createStudent("2026001", "张三", "tok-1")
	client := e.login("2026001", "tok-1")

	req, _ := http.NewRequest(http.MethodPut, e.server.URL+"/api/fs/write?path=/main.go", strings.NewReader("package main\n// snapshot me\n"))
	req.Header.Set("If-Match", "*")
	if r, err := client.Do(req); err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("write: %v %v", err, r)
	}

	res, body := doJSON(t, client, http.MethodPost, e.server.URL+"/api/snapshots", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("snapshot: %d %v", res.StatusCode, body)
	}
	if body["changed"] != true {
		t.Fatalf("expected changed snapshot: %v", body)
	}
	version := body["version"]
	if version == nil {
		t.Fatal("missing version")
	}
	snapDir := filepath.Join(e.data, "snapshots", "2026001", "v000001")
	if _, err := os.Stat(filepath.Join(snapDir, "main.go")); err != nil {
		t.Fatalf("snapshot file missing: %v", err)
	}

	// 同一内容再快照一次不应产生新版本。
	res, body = doJSON(t, client, http.MethodPost, e.server.URL+"/api/snapshots", nil, nil)
	if res.StatusCode != http.StatusOK || body["changed"] != false {
		t.Fatalf("repeat snapshot = %d %v", res.StatusCode, body)
	}

	// 审计里有写操作记录。
	audit, err := e.store.ListAudit(context.Background(), "2026001", 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audit {
		if a.Action == "write" && a.Path == "/main.go" && a.Status == http.StatusOK {
			found = true
		}
	}
	if !found {
		t.Fatalf("write not audited: %+v", audit)
	}
}

func TestJudgeRateLimit(t *testing.T) {
	e := newTestEnv(t)
	e.createStudent("2026001", "张三", "tok-1")
	client := e.login("2026001", "tok-1")
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		if err := e.store.InsertJudgeRun(ctx, store.JudgeRun{
			ID: store.NewID(), StudentNo: "2026001", Problem: "sum", Status: "done",
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	res, body := doJSON(t, client, http.MethodPost, e.server.URL+"/api/judge", map[string]string{"problem": "sum"}, nil)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (%v)", res.StatusCode, body)
	}
}

func TestStaticAndSPAFallback(t *testing.T) {
	e := newTestEnv(t)
	for _, p := range []string{"/", "/login"} {
		res, err := http.Get(e.server.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || !strings.Contains(string(b), "workbench") {
			t.Fatalf("%s -> %d %q", p, res.StatusCode, b)
		}
	}
}

func TestAdminCIDRRestriction(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspaces")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(root, "examd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	api := New(Config{
		WebDir:       root,
		WorkspaceDir: ws,
		AdminToken:   testAdminToken,
		AdminCIDRs:   []string{"10.0.0.0/8"},
	}, st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/admin/students", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for disallowed source network", res.StatusCode)
	}
}

func TestJudgeQueueDisabledWhenNil(t *testing.T) {
	e := newTestEnv(t)
	e.createStudent("2026001", "张三", "tok-1")
	client := e.login("2026001", "tok-1")
	res, _ := doJSON(t, client, http.MethodPost, e.server.URL+"/api/judge", map[string]string{"problem": "sum"}, nil)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
}

var _ = judge.Problem{}
var _ = workspace.ErrForbidden

func TestLoginRateLimit(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspaces")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(root, "examd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateStudent(context.Background(), "2026001", "张三", "tok-1"); err != nil {
		t.Fatal(err)
	}
	api := New(Config{
		WebDir:       root,
		WorkspaceDir: ws,
		AdminToken:   testAdminToken,
		LoginLimit:   2,
		LoginWindow:  300 * time.Millisecond,
	}, st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	attempt := func(token string) *http.Response {
		b, _ := json.Marshal(map[string]string{"student_no": "2026001", "token": token})
		res, err := http.Post(srv.URL+"/api/login", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res
	}

	for i := 0; i < 2; i++ {
		if res := attempt("wrong"); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, res.StatusCode)
		}
	}
	res := attempt("tok-1")
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("blocked attempt = %d, want 429", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}

	time.Sleep(350 * time.Millisecond)
	if res := attempt("tok-1"); res.StatusCode != http.StatusOK {
		t.Fatalf("after window = %d, want 200", res.StatusCode)
	}
}

func TestSecureCookieFlag(t *testing.T) {
	e := newTestEnv(t)
	e.createStudent("2026001", "张三", "tok-1")

	// 默认（纯 HTTP 内网）不加 Secure。
	b, _ := json.Marshal(map[string]string{"student_no": "2026001", "token": "tok-1"})
	res, err := http.Post(e.server.URL+"/api/login", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if strings.Contains(res.Header.Get("Set-Cookie"), "Secure") {
		t.Fatalf("unexpected Secure cookie: %s", res.Header.Get("Set-Cookie"))
	}

	// 打开 CookieSecure 后必须带 Secure。
	root := t.TempDir()
	ws := filepath.Join(root, "workspaces")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(root, "examd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateStudent(context.Background(), "2026002", "李四", "tok-2"); err != nil {
		t.Fatal(err)
	}
	api := New(Config{
		WebDir:       root,
		WorkspaceDir: ws,
		AdminToken:   testAdminToken,
		CookieSecure: true,
	}, st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	b, _ = json.Marshal(map[string]string{"student_no": "2026002", "token": "tok-2"})
	res, err = http.Post(srv.URL+"/api/login", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if !strings.Contains(res.Header.Get("Set-Cookie"), "Secure") {
		t.Fatalf("cookie not Secure: %s", res.Header.Get("Set-Cookie"))
	}
}

func TestAuditCSVExport(t *testing.T) {
	e := newTestEnv(t)
	e.createStudent("2026001", "张三", "tok-1")
	client := e.login("2026001", "tok-1")
	req, _ := http.NewRequest(http.MethodPut, e.server.URL+"/api/fs/write?path=/main.go", strings.NewReader("package main\n"))
	req.Header.Set("If-Match", "*")
	if r, err := client.Do(req); err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("write: %v %v", err, r)
	}

	req, _ = http.NewRequest(http.MethodGet, e.server.URL+"/api/admin/audit.csv?student=2026001", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content-type = %q", ct)
	}
	if !bytes.HasPrefix(body, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("missing UTF-8 BOM")
	}
	text := string(body)
	if !strings.Contains(text, "student_no,role,action,path,status,ip,detail") {
		t.Fatalf("missing header: %q", text[:min(120, len(text))])
	}
	if !strings.Contains(text, "2026001") || !strings.Contains(text, "/main.go") {
		t.Fatalf("missing audit row: %q", text)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
