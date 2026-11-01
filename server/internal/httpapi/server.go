// Package httpapi 提供 examd 的 HTTP 接口：登录、文件 API、快照、判题和静态资源。
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"exam.local/monaco-examd/internal/judge"
	"exam.local/monaco-examd/internal/snapshot"
	"exam.local/monaco-examd/internal/store"
)

// Config 是 HTTP 层的配置。
type Config struct {
	WebDir       string
	WorkspaceDir string
	SessionTTL   time.Duration
	MaxFileSize  int64
	AdminToken   string
	AdminCIDRs   []string
	// JudgeLimit 是每人每题允许的判题次数（默认 20）。
	JudgeLimit int
	// TrustProxy 为 true 时从 X-Forwarded-For 取客户端 IP。
	TrustProxy bool
}

// Server 把存储、快照、判题组合成 HTTP 服务。
type Server struct {
	cfg   Config
	st    *store.Store
	snap  *snapshot.Manager
	queue *judge.Queue
	log   *slog.Logger
	mux   *http.ServeMux
	admin []*net.IPNet

	gzipMu    sync.Mutex
	gzipCache sync.Map
}

// New 创建 HTTP 服务。
func New(cfg Config, st *store.Store, snap *snapshot.Manager, queue *judge.Queue, log *slog.Logger) *Server {
	if cfg.MaxFileSize <= 0 {
		cfg.MaxFileSize = 2 << 20
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.JudgeLimit <= 0 {
		cfg.JudgeLimit = 20
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Server{cfg: cfg, st: st, snap: snap, queue: queue, log: log, mux: http.NewServeMux()}
	for _, c := range cfg.AdminCIDRs {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(c)); err == nil {
			s.admin = append(s.admin, n)
		} else {
			log.Warn("ignoring invalid admin CIDR", "cidr", c)
		}
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	m := s.mux

	m.HandleFunc("GET /api/health", s.handleHealth)
	m.HandleFunc("GET /api/session", s.handleSession)
	m.HandleFunc("POST /api/login", s.handleLogin)
	m.HandleFunc("POST /api/logout", s.handleLogout)
	m.HandleFunc("GET /api/me", s.withStudent(s.handleMe))

	m.HandleFunc("GET /api/fs/list", s.withStudent(s.handleList))
	m.HandleFunc("GET /api/fs/stat", s.withStudent(s.handleStat))
	m.HandleFunc("GET /api/fs/read", s.withStudent(s.handleRead))
	m.HandleFunc("PUT /api/fs/write", s.withStudent(s.handleWrite))
	m.HandleFunc("POST /api/fs/mkdir", s.withStudent(s.handleMkdir))
	m.HandleFunc("POST /api/fs/rename", s.withStudent(s.handleRename))
	m.HandleFunc("DELETE /api/fs/delete", s.withStudent(s.handleDelete))

	m.HandleFunc("GET /api/snapshots", s.withStudent(s.handleListSnapshots))
	m.HandleFunc("POST /api/snapshots", s.withStudent(s.handleCreateSnapshot))

	m.HandleFunc("POST /api/judge", s.withStudent(s.handleSubmitJudge))
	m.HandleFunc("GET /api/judge/runs", s.withStudent(s.handleListJudgeRuns))
	m.HandleFunc("GET /api/judge/runs/{id}", s.withStudent(s.handleGetJudgeRun))
	m.HandleFunc("GET /api/problems", s.withStudent(s.handleListProblems))

	m.HandleFunc("POST /api/admin/login", s.handleAdminLogin)
	m.HandleFunc("POST /api/admin/logout", s.handleAdminLogout)
	m.HandleFunc("GET /api/admin/session", s.handleAdminSession)
	m.HandleFunc("GET /api/admin/students", s.withAdmin(s.handleAdminListStudents))
	m.HandleFunc("POST /api/admin/students", s.withAdmin(s.handleAdminCreateStudent))
	m.HandleFunc("POST /api/admin/students/reset-token", s.withAdmin(s.handleAdminResetToken))
	m.HandleFunc("POST /api/admin/students/disable", s.withAdmin(s.handleAdminDisable))
	m.HandleFunc("GET /api/admin/audit", s.withAdmin(s.handleAdminAudit))
	m.HandleFunc("GET /api/admin/snapshots", s.withAdmin(s.handleAdminSnapshots))
	m.HandleFunc("POST /api/admin/snapshots/restore", s.withAdmin(s.handleAdminRestore))
	m.HandleFunc("POST /api/admin/judge", s.withAdmin(s.handleAdminJudge))
	m.HandleFunc("GET /api/admin/judge/runs", s.withAdmin(s.handleAdminJudgeRuns))

	m.HandleFunc("GET /api/fs/workspace", s.withStudent(s.handleWorkspaceInfo))

	m.Handle("/", s.staticHandler())
}

// Handler 返回带中间件的 http.Handler。
func (s *Server) Handler() http.Handler {
	return s.recoverer(s.logging(s.securityHeaders(s.mux)))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"time": time.Now().UTC().Format(time.RFC3339),
	})
}

// handleSession 返回 200 而不是 401，前端用它判断是否需要显示登录框，
// 避免浏览器控制台里出现一条无意义的 401。
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if st, ok := s.currentStudent(r); ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": true,
			"student_no":    st.No,
			"name":          st.Name,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) ipAllowed(ipStr string) bool {
	if len(s.admin) == 0 {
		return true
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range s.admin {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ---- 中间件 ----

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "path", r.URL.Path, "panic", rec)
				writeError(w, http.StatusInternalServerError, "internal", "internal error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
				"ms", dur.Milliseconds(), "ip", s.clientIP(r))
		}
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// checkSameOrigin 拒绝跨站写请求（SameSite=Lax 之外的第二道防线）。
func (s *Server) checkSameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// ---- 身份 ----

type studentCtxKey struct{}

func (s *Server) withStudent(next func(http.ResponseWriter, *http.Request, store.Student)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkSameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross_origin", "cross-origin request rejected", nil)
			return
		}
		st, ok := s.currentStudent(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "login required", nil)
			return
		}
		next(w, r, st)
	}
}

func (s *Server) currentStudent(r *http.Request) (store.Student, bool) {
	no := ""
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		n, err := s.st.SessionStudent(r.Context(), c.Value)
		if err != nil {
			return store.Student{}, false
		}
		no = n
	} else if bearer := bearerToken(r); bearer != "" {
		// 允许工具/压测用 Bearer 令牌：学号 + 令牌通过 Basic 形式传更麻烦，这里用 X-Student-No 配合。
		no = r.Header.Get("X-Student-No")
		if no == "" {
			return store.Student{}, false
		}
		if _, err := s.st.Authenticate(r.Context(), no, bearer); err != nil {
			return store.Student{}, false
		}
	} else {
		return store.Student{}, false
	}
	st, err := s.st.GetStudent(r.Context(), no)
	if err != nil || st.Disabled {
		return store.Student{}, false
	}
	return st, true
}

func (s *Server) withAdmin(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkSameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross_origin", "cross-origin request rejected", nil)
			return
		}
		if !s.ipAllowed(s.clientIP(r)) {
			writeError(w, http.StatusForbidden, "ip_denied", "admin access is restricted to allowed networks", nil)
			return
		}
		if !s.isAdmin(r) {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "admin login required", nil)
			return
		}
		next(w, r)
	}
}

func (s *Server) isAdmin(r *http.Request) bool {
	if s.cfg.AdminToken == "" {
		return false
	}
	tok := bearerToken(r)
	if tok == "" {
		if c, err := r.Cookie(adminCookie); err == nil {
			tok = c.Value
		}
	}
	return tok != "" && subtleEqual(tok, s.cfg.AdminToken)
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// ---- 通用响应 ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string, extra map[string]any) {
	body := map[string]any{"error": msg, "code": code}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, status, body)
}

func decodeJSON(r *http.Request, v any, limit int64) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, limit))
	return dec.Decode(v)
}

var errTooLarge = errors.New("request body too large")

// ---- 会话 Cookie ----

const (
	sessionCookie = "exam_session"
	adminCookie   = "exam_admin"
)

func (s *Server) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   false, // 考场内网使用 HTTP；若前置 TLS 请改为 true。
		MaxAge:   maxAge,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.checkSameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross_origin", "cross-origin request rejected", nil)
		return
	}
	var req struct {
		StudentNo string `json:"student_no"`
		Token     string `json:"token"`
	}
	if err := decodeJSON(r, &req, 1<<16); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid json", nil)
		return
	}
	req.StudentNo = strings.TrimSpace(req.StudentNo)
	st, err := s.st.Authenticate(r.Context(), req.StudentNo, req.Token)
	if err != nil {
		s.st.Audit(r.Context(), store.AuditRecord{StudentNo: req.StudentNo, Role: "student", Action: "login",
			Status: http.StatusUnauthorized, IP: s.clientIP(r)})
		writeError(w, http.StatusUnauthorized, "bad_credentials", "invalid student_no or token", nil)
		return
	}
	sess, err := s.st.CreateSession(r.Context(), st.No, s.clientIP(r), s.cfg.SessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), nil)
		return
	}
	s.setCookie(w, sessionCookie, sess.ID, int(s.cfg.SessionTTL.Seconds()))
	s.st.Audit(r.Context(), store.AuditRecord{StudentNo: st.No, Role: "student", Action: "login",
		Status: http.StatusOK, IP: s.clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]any{
		"student_no": st.No,
		"name":       st.Name,
		"expires_at": sess.ExpiresAt,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.st.DeleteSession(r.Context(), c.Value)
	}
	s.setCookie(w, sessionCookie, "", -1)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, st store.Student) {
	writeJSON(w, http.StatusOK, map[string]any{
		"student_no": st.No,
		"name":       st.Name,
		"workspace":  "/",
	})
}

func (s *Server) handleWorkspaceInfo(w http.ResponseWriter, r *http.Request, st store.Student) {
	writeJSON(w, http.StatusOK, map[string]any{
		"student_no": st.No,
		"name":       st.Name,
		"root":       "/",
		"scheme":     "examfs",
	})
}

// ---- 管理员登录 ----

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if !s.checkSameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross_origin", "cross-origin request rejected", nil)
		return
	}
	if !s.ipAllowed(s.clientIP(r)) {
		writeError(w, http.StatusForbidden, "ip_denied", "admin access is restricted to allowed networks", nil)
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &req, 1<<16); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid json", nil)
		return
	}
	if s.cfg.AdminToken == "" || !subtleEqual(req.Token, s.cfg.AdminToken) {
		s.st.Audit(r.Context(), store.AuditRecord{Role: "admin", Action: "admin_login",
			Status: http.StatusUnauthorized, IP: s.clientIP(r)})
		writeError(w, http.StatusUnauthorized, "bad_credentials", "invalid admin token", nil)
		return
	}
	s.setCookie(w, adminCookie, req.Token, 8*3600)
	s.st.Audit(r.Context(), store.AuditRecord{Role: "admin", Action: "admin_login",
		Status: http.StatusOK, IP: s.clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	s.setCookie(w, adminCookie, "", -1)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAdminSession 与 /api/session 同理：未登录时返回 200 + authenticated=false。
func (s *Server) handleAdminSession(w http.ResponseWriter, r *http.Request) {
	allowed := s.ipAllowed(s.clientIP(r)) && s.isAdmin(r)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": allowed})
}
