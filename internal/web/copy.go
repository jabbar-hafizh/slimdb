package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jabbar-hafizh/slimdb/internal/db"
)

// A copy can take minutes, so it runs as a job the page polls.

const maxJobLog = 2000

type copyJob struct {
	mu       sync.Mutex
	ID       string    `json:"id"`
	Source   string    `json:"source"`
	Target   string    `json:"target"`
	Mode     string    `json:"mode"`
	State    string    `json:"state"` // running, done, failed, cancelled
	Error    string    `json:"error,omitempty"`
	Warnings []string  `json:"warnings"`
	Log      []string  `json:"log"`
	Dropped  int       `json:"dropped"` // log lines cut from the front
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	cancel   context.CancelFunc
}

func (j *copyJob) logf(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Log = append(j.Log, line)
	if over := len(j.Log) - maxJobLog; over > 0 {
		j.Log = append([]string(nil), j.Log[over:]...)
		j.Dropped += over
	}
}

type copyJobs struct {
	mu   sync.Mutex
	jobs map[string]*copyJob
	seq  int
}

type copyRequest struct {
	Source  string   `json:"source"`
	Target  string   `json:"target"`
	Mode    string   `json:"mode"`
	Schemas []string `json:"schemas"`
	// Confirm must repeat the target's name when it is not on this machine.
	Confirm string `json:"confirm"`
}

func (s *Server) startCopy(w http.ResponseWriter, r *http.Request) {
	var req copyRequest
	if !readJSON(w, r, &req) {
		return
	}
	src, err := s.store.Conn(req.Source)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("source: %w", err))
		return
	}
	dst, err := s.store.Conn(req.Target)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("target: %w", err))
		return
	}
	opt := db.CopyOptions{Mode: req.Mode, Schemas: req.Schemas}
	if err := db.CheckCopy(src, dst, opt); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !db.IsLocalTarget(dst) && strings.TrimSpace(req.Confirm) != dst.Name {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%q is not on this machine; type its name to confirm overwriting it", dst.Name))
		return
	}

	// The job outlives the request, so it gets its own context.
	ctx, cancel := context.WithCancel(context.Background())
	s.copies.mu.Lock()
	if s.copies.jobs == nil {
		s.copies.jobs = map[string]*copyJob{}
	}
	for _, j := range s.copies.jobs {
		j.mu.Lock()
		busy := j.State == "running" && (j.Target == dst.Name)
		j.mu.Unlock()
		if busy {
			s.copies.mu.Unlock()
			cancel()
			writeError(w, http.StatusConflict, fmt.Errorf("a copy into %q is already running", dst.Name))
			return
		}
	}
	s.copies.seq++
	job := &copyJob{
		ID: fmt.Sprintf("copy-%d", s.copies.seq), Source: src.Name, Target: dst.Name, Mode: req.Mode,
		State: "running", Warnings: []string{}, Log: []string{}, Started: time.Now(), cancel: cancel,
	}
	s.copies.jobs[job.ID] = job
	s.copies.mu.Unlock()

	go func() {
		defer cancel()
		report, err := db.Copy(ctx, src, dst, opt, job.logf)
		// Cached drivers may hold schema the copy just replaced.
		s.dbs.Forget(dst.ID)
		job.mu.Lock()
		defer job.mu.Unlock()
		job.Warnings = report.Warnings
		job.Finished = time.Now()
		switch {
		case errors.Is(err, context.Canceled):
			job.State = "cancelled"
		case err != nil:
			job.State, job.Error = "failed", err.Error()
		default:
			job.State = "done"
		}
	}()
	writeJSON(w, http.StatusAccepted, s.jobView(job))
}

func (s *Server) getCopy(w http.ResponseWriter, r *http.Request) {
	job := s.findJob(r.PathValue("id"))
	if job == nil {
		writeError(w, http.StatusNotFound, errors.New("no such copy"))
		return
	}
	writeJSON(w, http.StatusOK, s.jobView(job))
}

func (s *Server) cancelCopy(w http.ResponseWriter, r *http.Request) {
	job := s.findJob(r.PathValue("id"))
	if job == nil {
		writeError(w, http.StatusNotFound, errors.New("no such copy"))
		return
	}
	job.cancel()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) findJob(id string) *copyJob {
	s.copies.mu.Lock()
	defer s.copies.mu.Unlock()
	return s.copies.jobs[id]
}

// jobView copies the job under its lock, so encoding never races the runner.
func (s *Server) jobView(j *copyJob) map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]any{
		"id": j.ID, "source": j.Source, "target": j.Target, "mode": j.Mode, "state": j.State,
		"error": j.Error, "warnings": append([]string{}, j.Warnings...),
		"log": append([]string{}, j.Log...), "dropped": j.Dropped,
		"started": j.Started, "finished": j.Finished,
	}
}
