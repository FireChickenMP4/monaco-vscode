package httpapi

import (
	"encoding/csv"
	"net/http"
	"sort"
	"strconv"
	"time"

	"exam.local/monaco-examd/internal/store"
)

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request, st store.Student) {
	list, err := s.st.ListSnapshots(r.Context(), st.No, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": list})
}

func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request, st store.Student) {
	if s.snap == nil {
		writeError(w, http.StatusServiceUnavailable, "disabled", "snapshot service disabled", nil)
		return
	}
	res, err := s.snap.SnapshotOne(r.Context(), st.No)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), nil)
		return
	}
	s.audit(st, r, "snapshot", "/", http.StatusOK, "")
	writeJSON(w, http.StatusOK, res)
}

type problemSummary struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	TimeLimitMS int    `json:"time_limit_ms"`
	MemoryMB    int    `json:"memory_mb"`
	Cases       int    `json:"cases"`
}

func (s *Server) problemSummaries() []problemSummary {
	out := []problemSummary{}
	if s.queue == nil || s.queue.Judge == nil {
		return out
	}
	for id, p := range s.queue.Judge.Problems {
		out = append(out, problemSummary{ID: id, Title: p.Title, TimeLimitMS: p.TimeLimitMS, MemoryMB: p.MemoryMB, Cases: len(p.Cases)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Server) handleListProblems(w http.ResponseWriter, r *http.Request, st store.Student) {
	writeJSON(w, http.StatusOK, map[string]any{"problems": s.problemSummaries()})
}

func (s *Server) submitJudge(w http.ResponseWriter, r *http.Request, studentNo, problemID string) {
	if s.queue != nil {
		if _, ok := s.queue.Judge.Problems[problemID]; !ok {
			writeError(w, http.StatusNotFound, "unknown_problem", "unknown problem", nil)
			return
		}
	}
	count, err := s.st.CountJudgeRuns(r.Context(), studentNo, problemID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), nil)
		return
	}
	if count >= s.cfg.JudgeLimit {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "judge attempt limit reached for this problem", nil)
		return
	}
	if s.queue == nil {
		writeError(w, http.StatusServiceUnavailable, "disabled", "judge service disabled", nil)
		return
	}
	run, err := s.queue.Submit(r.Context(), studentNo, problemID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "busy", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleSubmitJudge(w http.ResponseWriter, r *http.Request, st store.Student) {
	var req struct {
		Problem string `json:"problem"`
	}
	if err := decodeJSON(r, &req, 1<<16); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid json", nil)
		return
	}
	s.audit(st, r, "judge", req.Problem, http.StatusAccepted, "")
	s.submitJudge(w, r, st.No, req.Problem)
}

func (s *Server) handleListJudgeRuns(w http.ResponseWriter, r *http.Request, st store.Student) {
	runs, err := s.st.ListJudgeRuns(r.Context(), st.No, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) handleGetJudgeRun(w http.ResponseWriter, r *http.Request, st store.Student) {
	id := r.PathValue("id")
	run, err := s.st.GetJudgeRun(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found", nil)
		return
	}
	if run.StudentNo != st.No {
		writeError(w, http.StatusForbidden, "forbidden", "not your run", nil)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// ---- 管理端 ----

func (s *Server) handleAdminListStudents(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListStudents(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"students": list})
}

func (s *Server) handleAdminCreateStudent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StudentNo string `json:"student_no"`
		Name      string `json:"name"`
		Token     string `json:"token"`
	}
	if err := decodeJSON(r, &req, 1<<16); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid json", nil)
		return
	}
	if req.Token == "" {
		req.Token = store.NewToken()
	}
	st, err := s.st.CreateStudent(r.Context(), req.StudentNo, req.Name, req.Token)
	if err != nil {
		writeError(w, http.StatusConflict, "conflict", err.Error(), nil)
		return
	}
	if _, err := s.provision(st.No); err != nil {
		s.log.Warn("provision failed", "student", st.No, "err", err)
	}
	s.st.Audit(r.Context(), store.AuditRecord{StudentNo: st.No, Role: "admin", Action: "create_student",
		Status: http.StatusCreated, IP: s.clientIP(r)})
	writeJSON(w, http.StatusCreated, map[string]any{"student_no": st.No, "name": st.Name, "token": req.Token})
}

func (s *Server) handleAdminResetToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StudentNo string `json:"student_no"`
	}
	if err := decodeJSON(r, &req, 1<<16); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid json", nil)
		return
	}
	token := store.NewToken()
	if err := s.st.SetStudentToken(r.Context(), req.StudentNo, token); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "student not found", nil)
		return
	}
	s.st.Audit(r.Context(), store.AuditRecord{StudentNo: req.StudentNo, Role: "admin", Action: "reset_token",
		Status: http.StatusOK, IP: s.clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]any{"student_no": req.StudentNo, "token": token})
}

func (s *Server) handleAdminDisable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StudentNo string `json:"student_no"`
		Disabled  bool   `json:"disabled"`
	}
	if err := decodeJSON(r, &req, 1<<16); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid json", nil)
		return
	}
	if err := s.st.SetStudentDisabled(r.Context(), req.StudentNo, req.Disabled); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "student not found", nil)
		return
	}
	s.st.Audit(r.Context(), store.AuditRecord{StudentNo: req.StudentNo, Role: "admin", Action: "disable",
		Status: http.StatusOK, IP: s.clientIP(r), Detail: boolText(req.Disabled)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	studentNo := r.URL.Query().Get("student")
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := parseInt(v); err == nil {
			limit = n
		}
	}
	list, err := s.st.ListAudit(r.Context(), studentNo, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": list})
}

// handleAdminAuditCSV 导出审计记录，带 UTF-8 BOM 以便 Excel 直接打开。
func (s *Server) handleAdminAuditCSV(w http.ResponseWriter, r *http.Request) {
	studentNo := r.URL.Query().Get("student")
	limit := 50000
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := parseInt(v); err == nil && n > 0 {
			limit = n
		}
	}
	list, err := s.st.ListAudit(r.Context(), studentNo, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), nil)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="audit.csv"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "at", "student_no", "role", "action", "path", "status", "ip", "detail"})
	for _, a := range list {
		_ = cw.Write([]string{
			strconv.FormatInt(a.ID, 10),
			a.At.Format(time.RFC3339),
			a.StudentNo,
			a.Role,
			a.Action,
			a.Path,
			strconv.Itoa(a.Status),
			a.IP,
			a.Detail,
		})
	}
	cw.Flush()
}

func (s *Server) handleAdminSnapshots(w http.ResponseWriter, r *http.Request) {
	studentNo := r.URL.Query().Get("student")
	if studentNo == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "student required", nil)
		return
	}
	list, err := s.st.ListSnapshots(r.Context(), studentNo, 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), nil)
		return
	}
	var latest any
	if l, err := s.st.LatestSnapshot(r.Context(), studentNo); err == nil {
		latest = l
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": list, "latest": latest})
}

func (s *Server) handleAdminRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StudentNo string `json:"student_no"`
		Version   int    `json:"version"`
	}
	if err := decodeJSON(r, &req, 1<<16); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid json", nil)
		return
	}
	if s.snap == nil {
		writeError(w, http.StatusServiceUnavailable, "disabled", "snapshot service disabled", nil)
		return
	}
	if err := s.snap.Restore(r.Context(), req.StudentNo, req.Version); err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error(), nil)
		return
	}
	s.st.Audit(r.Context(), store.AuditRecord{StudentNo: req.StudentNo, Role: "admin", Action: "restore_snapshot",
		Status: http.StatusOK, IP: s.clientIP(r), Detail: intText(req.Version)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminJudge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StudentNo string `json:"student_no"`
		Problem   string `json:"problem"`
	}
	if err := decodeJSON(r, &req, 1<<16); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid json", nil)
		return
	}
	s.submitJudge(w, r, req.StudentNo, req.Problem)
}

func (s *Server) handleAdminJudgeRuns(w http.ResponseWriter, r *http.Request) {
	studentNo := r.URL.Query().Get("student")
	runs, err := s.st.ListJudgeRuns(r.Context(), studentNo, 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) provision(studentNo string) (string, error) {
	return provisionWorkspace(s.cfg.WorkspaceDir, studentNo)
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
