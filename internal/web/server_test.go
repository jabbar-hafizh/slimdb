package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jabbar-hafizh/slimdb/internal/db"
	"github.com/jabbar-hafizh/slimdb/internal/store"
)

func newTestServer(t *testing.T) *Server {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(st, db.NewManager())
}

func TestGuard(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	tests := []struct {
		name   string
		host   string
		path   string
		token  string
		status int
	}{
		{"page on localhost", "localhost:7788", "/", "", http.StatusOK},
		{"page on 127.0.0.1", "127.0.0.1:7788", "/", "", http.StatusOK},
		{"rebinding host refused", "evil.example:7788", "/", "", http.StatusForbidden},
		{"api without token", "localhost:7788", "/api/connections", "", http.StatusUnauthorized},
		{"api with wrong token", "localhost:7788", "/api/connections", "nope", http.StatusUnauthorized},
		{"api with token", "localhost:7788", "/api/connections", s.token, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Host = tt.host
			if tt.token != "" {
				req.Header.Set(tokenHeader, tt.token)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tt.status, rec.Body.String())
			}
		})
	}
}

func TestIndexEmbedsToken(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), s.token) || strings.Contains(rec.Body.String(), "{{TOKEN}}") {
		t.Error("index.html does not carry the session token")
	}
}
