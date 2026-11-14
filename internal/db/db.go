// Package db holds the database drivers behind one small interface, so the
// web UI and the MCP server treat Postgres and MongoDB the same way.
package db

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/jabbar-hafizh/slimdb/internal/store"
)

// Result is one statement's output, shaped as a table.
type Result struct {
	Title     string   `json:"title,omitempty"`
	Statement string   `json:"statement,omitempty"`
	Columns   []Column `json:"columns"`
	Rows      [][]any  `json:"rows"`
	// Docs holds each MongoDB document as relaxed Extended JSON, for the JSON view.
	Docs      []string `json:"docs,omitempty"`
	Affected  int64    `json:"affected"`
	Truncated bool     `json:"truncated"`
	Millis    int64    `json:"ms"`
	Message   string   `json:"message,omitempty"`
	Error     string   `json:"error,omitempty"`
}

type Column struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// Node is one entry in the object tree: a schema or database, and its tables or collections.
type Node struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Object   string `json:"object,omitempty"` // what Describe accepts, e.g. "public.users"
	Database string `json:"database,omitempty"`
	Children []Node `json:"children,omitempty"`
}

// Driver is implemented once per database kind.
//
// Query runs every statement in q in order and stops at the first failing
// one, whose Result carries the error. The returned error is reserved for
// failures that are not tied to a statement, such as a lost connection.
type Driver interface {
	Tree(ctx context.Context) ([]Node, error)
	Describe(ctx context.Context, database, object string) ([]Result, error)
	Query(ctx context.Context, database, q string, limit int) ([]Result, error)
	Explain(ctx context.Context, database, q string, analyze bool) ([]Result, error)
	Close()
}

// Open connects to c. When SLIMDB_LOCALHOST_ALIAS is set (the Docker image
// sets it), localhost in the URL is replaced by that host first.
func Open(ctx context.Context, c store.Conn) (Driver, error) {
	url := rewriteLocalhost(c.URL, os.Getenv("SLIMDB_LOCALHOST_ALIAS"))
	switch c.Kind {
	case store.KindPostgres:
		return openPostgres(ctx, url, c.ReadOnly)
	case store.KindMongo:
		return openMongo(ctx, url, c.ReadOnly)
	}
	return nil, fmt.Errorf("unknown connection kind %q", c.Kind)
}

// Manager keeps one open driver per saved connection and reopens it when
// the connection's settings change.
type Manager struct {
	mu   sync.Mutex
	open map[string]openDriver
}

type openDriver struct {
	conn   store.Conn
	driver Driver
}

func NewManager() *Manager { return &Manager{open: map[string]openDriver{}} }

func (m *Manager) Get(ctx context.Context, c store.Conn) (Driver, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.open[c.ID]; ok {
		if o.conn == c {
			return o.driver, nil
		}
		o.driver.Close()
		delete(m.open, c.ID)
	}
	d, err := Open(ctx, c)
	if err != nil {
		return nil, err
	}
	m.open[c.ID] = openDriver{conn: c, driver: d}
	return d, nil
}

func (m *Manager) Forget(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.open[id]; ok {
		o.driver.Close()
		delete(m.open, id)
	}
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, o := range m.open {
		o.driver.Close()
		delete(m.open, id)
	}
}
