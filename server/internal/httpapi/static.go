package httpapi

import (
	"bytes"
	"compress/gzip"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type gzEntry struct {
	size    int64
	modTime time.Time
	data    []byte
}

// staticHandler 提供 workbench 静态资源，未命中的非 API 路径回落到 index.html。
func (s *Server) staticHandler() http.Handler {
	dir := s.cfg.WebDir
	fs := http.Dir(dir)
	fileServer := http.FileServer(fs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, "not_found", "no such endpoint", nil)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", nil)
			return
		}
		clean := path.Clean("/" + r.URL.Path)
		if clean == "/" {
			s.serveIndex(w, r, dir)
			return
		}
		full := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
		if fi, err := os.Stat(full); err == nil && !fi.IsDir() {
			if strings.HasPrefix(clean, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			if maybeGzip(r) {
				s.serveGzip(w, r, full, fileServer)
				return
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA 回退（例如直接访问 /login）。
		s.serveIndex(w, r, dir)
	})
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request, dir string) {
	index := filepath.Join(dir, "index.html")
	f, err := os.Open(index)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("workbench assets not built: run `npm run build` in web/ (expected " + index + ")\n"))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

func maybeGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

func (s *Server) serveGzip(w http.ResponseWriter, r *http.Request, full string, next http.Handler) {
	switch strings.ToLower(filepath.Ext(full)) {
	case ".js", ".mjs", ".css", ".html", ".json", ".svg", ".ttf", ".map":
	default:
		next.ServeHTTP(w, r)
		return
	}
	fi, err := os.Stat(full)
	if err != nil {
		next.ServeHTTP(w, r)
		return
	}
	// 同一份静态资源只压缩一次；70 个客户端同时首次加载时不能各压一遍。
	data := s.cachedGzip(full, fi)
	if data == nil {
		next.ServeHTTP(w, r)
		return
	}
	if ct := mime.TypeByExtension(filepath.Ext(full)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Vary", "Accept-Encoding")
	if r.URL.Path != "/" && !strings.HasPrefix(path.Clean(r.URL.Path), "/assets/") {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// cachedGzip 返回压缩后的内容，按 (路径, 大小, mtime) 缓存。
func (s *Server) cachedGzip(full string, fi os.FileInfo) []byte {
	if v, ok := s.gzipCache.Load(full); ok {
		e := v.(gzEntry)
		if e.size == fi.Size() && e.modTime.Equal(fi.ModTime()) {
			return e.data
		}
	}
	s.gzipMu.Lock()
	defer s.gzipMu.Unlock()
	if v, ok := s.gzipCache.Load(full); ok {
		e := v.(gzEntry)
		if e.size == fi.Size() && e.modTime.Equal(fi.ModTime()) {
			return e.data
		}
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return nil
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		return nil
	}
	if err := gz.Close(); err != nil {
		return nil
	}
	out := buf.Bytes()
	if len(out) < 32<<20 {
		s.gzipCache.Store(full, gzEntry{size: fi.Size(), modTime: fi.ModTime(), data: out})
	}
	return out
}
