package httpapi

import (
	"crypto/subtle"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"exam.local/monaco-examd/internal/store"
	"exam.local/monaco-examd/internal/workspace"
)

func subtleEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *Server) studentRoot(st store.Student) string {
	return filepath.Join(s.cfg.WorkspaceDir, st.No)
}

func queryPath(r *http.Request) string {
	p := r.URL.Query().Get("path")
	if p == "" {
		return "/"
	}
	return p
}

// fsError 把工作区错误映射为 HTTP 状态码。
func (s *Server) fsError(w http.ResponseWriter, err error) {
	var conflict *workspace.ConflictError
	switch {
	case errors.As(err, &conflict):
		writeError(w, http.StatusConflict, "etag_mismatch", "file changed on the server", map[string]any{
			"current_etag": conflict.Current,
		})
	case errors.Is(err, workspace.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "path is not allowed", nil)
	case errors.Is(err, workspace.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "path not found", nil)
	case errors.Is(err, workspace.ErrPrecondition):
		writeError(w, http.StatusPreconditionRequired, "precondition_required", "If-Match header is required", nil)
	case errors.Is(err, workspace.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "target already exists", nil)
	case errors.Is(err, workspace.ErrIsDir):
		writeError(w, http.StatusConflict, "is_directory", "path is a directory", nil)
	case errors.Is(err, workspace.ErrNotDir):
		writeError(w, http.StatusConflict, "not_directory", "path is not a directory", nil)
	case errors.Is(err, workspace.ErrNotEmpty):
		writeError(w, http.StatusConflict, "not_empty", "directory is not empty", nil)
	case errors.Is(err, workspace.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "file too large", nil)
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, "not_found", "path not found", nil)
	default:
		s.log.Error("fs error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
}

func (s *Server) audit(st store.Student, r *http.Request, action, p string, status int, detail string) {
	s.st.Audit(r.Context(), store.AuditRecord{
		StudentNo: st.No,
		Role:      "student",
		Action:    action,
		Path:      p,
		Status:    status,
		IP:        s.clientIP(r),
		Detail:    detail,
	})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request, st store.Student) {
	p := queryPath(r)
	entries, err := workspace.List(s.studentRoot(st), p)
	if err != nil {
		s.fsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "entries": entries})
}

func (s *Server) handleStat(w http.ResponseWriter, r *http.Request, st store.Student) {
	e, err := workspace.Stat(s.studentRoot(st), queryPath(r))
	if err != nil {
		s.fsError(w, err)
		return
	}
	if e.ETag != "" {
		w.Header().Set("ETag", e.ETag)
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request, st store.Student) {
	data, e, err := workspace.Read(s.studentRoot(st), queryPath(r), s.cfg.MaxFileSize)
	if err != nil {
		s.fsError(w, err)
		return
	}
	w.Header().Set("ETag", e.ETag)
	ct := mime.TypeByExtension(path.Ext(e.Name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	if strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "xml") {
		ct += "; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-File-Mtime", e.Mtime.Format("2006-01-02T15:04:05.000Z"))
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == e.ETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(data)), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request, st store.Student) {
	p := queryPath(r)
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxFileSize+1)
	defer body.Close()
	data, err := readAll(body, s.cfg.MaxFileSize)
	if err != nil {
		if errors.Is(err, workspace.ErrTooLarge) {
			s.audit(st, r, "write", p, http.StatusRequestEntityTooLarge, "too large")
			s.fsError(w, err)
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "cannot read body", nil)
		return
	}
	ifMatch := r.Header.Get("If-Match")
	e, err := workspace.Write(s.studentRoot(st), p, data, ifMatch, s.cfg.MaxFileSize)
	if err != nil {
		status := statusForFSError(err)
		s.audit(st, r, "write", p, status, err.Error())
		s.fsError(w, err)
		return
	}
	s.audit(st, r, "write", p, http.StatusOK, strconv.Itoa(len(data))+" bytes")
	w.Header().Set("ETag", e.ETag)
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request, st store.Student) {
	p := queryPath(r)
	if r.ContentLength > 0 {
		var req struct {
			Path string `json:"path"`
		}
		if err := decodeJSON(r, &req, 1<<16); err == nil && req.Path != "" {
			p = req.Path
		}
	}
	e, err := workspace.Mkdir(s.studentRoot(st), p)
	if err != nil {
		s.audit(st, r, "mkdir", p, statusForFSError(err), err.Error())
		s.fsError(w, err)
		return
	}
	s.audit(st, r, "mkdir", p, http.StatusOK, "")
	writeJSON(w, http.StatusCreated, e)
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request, st store.Student) {
	var req struct {
		From      string `json:"from"`
		To        string `json:"to"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := decodeJSON(r, &req, 1<<16); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid json", nil)
		return
	}
	e, err := workspace.Rename(s.studentRoot(st), req.From, req.To, req.Overwrite)
	if err != nil {
		s.audit(st, r, "rename", req.From+" -> "+req.To, statusForFSError(err), err.Error())
		s.fsError(w, err)
		return
	}
	s.audit(st, r, "rename", req.From+" -> "+req.To, http.StatusOK, "")
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, st store.Student) {
	p := queryPath(r)
	recursive := r.URL.Query().Get("recursive") == "1" || r.URL.Query().Get("recursive") == "true"
	err := workspace.Remove(s.studentRoot(st), p, recursive)
	if err != nil {
		s.audit(st, r, "delete", p, statusForFSError(err), err.Error())
		s.fsError(w, err)
		return
	}
	s.audit(st, r, "delete", p, http.StatusOK, "")
	w.WriteHeader(http.StatusNoContent)
}

func statusForFSError(err error) int {
	var conflict *workspace.ConflictError
	switch {
	case errors.As(err, &conflict), errors.Is(err, workspace.ErrConflict),
		errors.Is(err, workspace.ErrIsDir), errors.Is(err, workspace.ErrNotDir),
		errors.Is(err, workspace.ErrNotEmpty):
		return http.StatusConflict
	case errors.Is(err, workspace.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, workspace.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, workspace.ErrPrecondition):
		return http.StatusPreconditionRequired
	case errors.Is(err, workspace.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusInternalServerError
}

// readAll 读取请求体，超过 max 时返回 ErrTooLarge。
func readAll(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, workspace.ErrTooLarge
	}
	return data, nil
}
