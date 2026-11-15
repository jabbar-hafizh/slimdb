package web

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jabbar-hafizh/slimdb/internal/db"
)

func dumpOptions(r *http.Request) db.CopyOptions {
	opt := db.CopyOptions{Mode: r.URL.Query().Get("mode")}
	if opt.Mode == "" {
		opt.Mode = db.CopyAll
	}
	for _, sc := range strings.Split(r.URL.Query().Get("schemas"), ",") {
		if sc = strings.TrimSpace(sc); sc != "" {
			opt.Schemas = append(opt.Schemas, sc)
		}
	}
	return opt
}

// checkDump lets the page find problems (missing pg_dump, a database that
// does not answer) before it starts a download, which cannot show errors.
func (s *Server) checkDump(w http.ResponseWriter, r *http.Request) {
	d, c, ok := s.driver(w, r)
	if !ok {
		return
	}
	opt := dumpOptions(r)
	if err := db.CheckDump(c, opt); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := d.Tree(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"file": db.DumpFileName(c, opt.Mode, time.Now())})
}

// dump streams the dump as a download. The browser saves it in its download
// folder. An error halfway aborts the response, so the browser marks the
// download as failed instead of keeping a cut-off file.
func (s *Server) dump(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.Conn(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	opt := dumpOptions(r)
	if err := db.CheckDump(c, opt); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name := db.DumpFileName(c, opt.Mode, time.Now())
	sw := &startWriter{w: w, start: func() {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}}
	err = db.Dump(r.Context(), c, opt, sw, func(string) {})
	if err == nil {
		sw.ensureStarted()
		return
	}
	if !sw.started {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	panic(http.ErrAbortHandler)
}

// startWriter sets the download headers on the first byte, so an error
// before any output can still be a normal error response.
type startWriter struct {
	w       http.ResponseWriter
	start   func()
	started bool
}

func (sw *startWriter) ensureStarted() {
	if !sw.started {
		sw.started = true
		sw.start()
	}
}

func (sw *startWriter) Write(p []byte) (int, error) {
	sw.ensureStarted()
	return sw.w.Write(p)
}

// importDump receives a dump file as the request body, saves it to a temp
// file and restores it as a job, like a copy.
func (s *Server) importDump(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.Conn(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	opt := dumpOptions(r)
	if c.ReadOnly {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%q is read-only", c.Name))
		return
	}
	if !db.IsLocalTarget(c) && strings.TrimSpace(r.URL.Query().Get("confirm")) != c.Name {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%q is not on this machine; type its name to confirm overwriting it", c.Name))
		return
	}
	file := filepath.Base(r.URL.Query().Get("file"))
	tmp, err := os.CreateTemp("", "slimdb-import-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	cleanup := func() { tmp.Close(); os.Remove(tmp.Name()) }
	if _, err := io.Copy(tmp, r.Body); err != nil {
		cleanup()
		writeError(w, http.StatusBadRequest, fmt.Errorf("upload: %w", err))
		return
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	job, err := s.startJob(file, c, opt.Mode, func(ctx context.Context, logf func(string)) (db.CopyReport, error) {
		return db.Restore(ctx, c, opt, tmp.Name(), logf)
	}, cleanup)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.jobView(job))
}
