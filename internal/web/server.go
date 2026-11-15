// Package web serves the browser UI and the JSON API behind it.
package web

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jabbar-hafizh/slimdb/internal/db"
	"github.com/jabbar-hafizh/slimdb/internal/store"
)

//go:embed static
var staticFiles embed.FS

const (
	defaultLimit = 500
	maxLimit     = 100_000
	tokenHeader  = "X-Slimdb-Token"
)

type Server struct {
	store *store.Store
	dbs   *db.Manager
	// token is generated per run and embedded in the page. Requiring it on
	// every API call stops other websites from driving the API through the
	// browser (CSRF); the Host check below stops DNS rebinding.
	token  string
	copies copyJobs
}

func New(st *store.Store, dbs *db.Manager) *Server {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return &Server{store: st, dbs: dbs, token: hex.EncodeToString(b)}
}

func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(staticFiles, "static")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.HandleFunc("GET /api/connections", s.listConnections)
	mux.HandleFunc("POST /api/connections", s.saveConnection)
	mux.HandleFunc("POST /api/connections/test", s.testConnection)
	mux.HandleFunc("DELETE /api/connections/{id}", s.deleteConnection)
	mux.HandleFunc("GET /api/connections/{id}/tree", s.tree)
	mux.HandleFunc("GET /api/connections/{id}/describe", s.describe)
	mux.HandleFunc("POST /api/connections/{id}/query", s.query)
	mux.HandleFunc("POST /api/connections/{id}/explain", s.explain)
	mux.HandleFunc("GET /api/history", s.history)
	mux.HandleFunc("POST /api/copy", s.startCopy)
	mux.HandleFunc("GET /api/copy/{id}", s.getCopy)
	mux.HandleFunc("POST /api/copy/{id}/cancel", s.cancelCopy)
	return s.guard(mux)
}

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, "slimdb only answers on localhost", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get(tokenHeader)), []byte(s.token)) != 1 {
			writeError(w, http.StatusUnauthorized, errors.New("missing or wrong token, reload the page"))
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	page, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	html := strings.Replace(string(page), "{{TOKEN}}", s.token, 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(html))
}

func (s *Server) listConnections(w http.ResponseWriter, _ *http.Request) {
	conns, err := s.store.Conns()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, conns)
}

func (s *Server) saveConnection(w http.ResponseWriter, r *http.Request) {
	var c store.Conn
	if !readJSON(w, r, &c) {
		return
	}
	saved, err := s.store.Save(c)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) testConnection(w http.ResponseWriter, r *http.Request) {
	var c store.Conn
	if !readJSON(w, r, &c) {
		return
	}
	kind, err := store.KindFromURL(strings.TrimSpace(c.URL))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	c.Kind, c.URL = kind, strings.TrimSpace(c.URL)
	start := time.Now()
	d, err := db.Open(r.Context(), c)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	d.Close()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ms": time.Since(start).Milliseconds()})
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.Delete(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	s.dbs.Forget(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) tree(w http.ResponseWriter, r *http.Request) {
	d, _, ok := s.driver(w, r)
	if !ok {
		return
	}
	nodes, err := d.Tree(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, nodes)
}

func (s *Server) describe(w http.ResponseWriter, r *http.Request) {
	d, _, ok := s.driver(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	results, err := d.Describe(r.Context(), q.Get("database"), q.Get("object"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

type queryRequest struct {
	Query    string `json:"query"`
	Database string `json:"database"`
	Limit    int    `json:"limit"`
	Analyze  bool   `json:"analyze"`
}

func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	d, c, ok := s.driver(w, r)
	if !ok {
		return
	}
	var req queryRequest
	if !readJSON(w, r, &req) {
		return
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maxLimit)

	start := time.Now()
	results, err := d.Query(r.Context(), req.Database, req.Query, limit)
	entry := store.HistoryEntry{
		ConnID: c.ID, Database: req.Database, Query: req.Query, Source: "ui",
		At: start, Millis: time.Since(start).Milliseconds(),
	}
	if err != nil {
		entry.Error = err.Error()
	} else if len(results) > 0 {
		entry.Error = results[len(results)-1].Error
	}
	s.store.AddHistory(entry)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Server) explain(w http.ResponseWriter, r *http.Request) {
	d, _, ok := s.driver(w, r)
	if !ok {
		return
	}
	var req queryRequest
	if !readJSON(w, r, &req) {
		return
	}
	results, err := d.Explain(r.Context(), req.Database, req.Query, req.Analyze)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	entries, err := s.store.History(r.URL.Query().Get("conn"), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// driver resolves the {id} path value to an open driver, writing the error response itself.
func (s *Server) driver(w http.ResponseWriter, r *http.Request) (db.Driver, store.Conn, bool) {
	c, err := s.store.Conn(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return nil, c, false
	}
	d, err := s.dbs.Get(r.Context(), c)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return nil, c, false
	}
	return d, c, true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 10<<20))
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
