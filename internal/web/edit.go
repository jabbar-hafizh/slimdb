package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jabbar-hafizh/slimdb/internal/db"
	"github.com/jabbar-hafizh/slimdb/internal/store"
)

type editRequest struct {
	Database string `json:"database"`
	db.Changes
}

// edit applies changes made in the results grid.
func (s *Server) edit(w http.ResponseWriter, r *http.Request) {
	d, c, ok := s.driver(w, r)
	if !ok {
		return
	}
	var req editRequest
	if !readJSON(w, r, &req) {
		return
	}
	if c.ReadOnly {
		writeError(w, http.StatusForbidden, db.ErrReadOnly)
		return
	}
	ed, ok := d.(db.Editor)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s results cannot be edited in the grid", c.Kind))
		return
	}
	if len(req.Ops) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("nothing to save"))
		return
	}
	start := time.Now()
	res, err := ed.Apply(r.Context(), req.Database, req.Changes)
	// History keeps a readable record of what the grid changed.
	ops, _ := json.Marshal(req.Changes)
	entry := store.HistoryEntry{
		ConnID: c.ID, Database: req.Database, Query: "-- grid edit\n" + string(ops), Source: "ui",
		At: start, Millis: time.Since(start).Milliseconds(),
	}
	if err != nil {
		entry.Error = err.Error()
	}
	s.store.AddHistory(entry)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
