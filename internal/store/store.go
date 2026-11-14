// Package store keeps saved connections and query history on local disk.
//
// The web UI and the MCP server run as separate processes, so every call
// reloads from disk instead of caching. Connections live in connections.json
// (written atomically), history in an append-only history.jsonl.
package store

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	KindPostgres = "postgres"
	KindMongo    = "mongo"

	historyKeep = 500
	historyTrim = 2000 // trim the file back to historyKeep once it grows past this
)

type Conn struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	URL      string `json:"url"`
	ReadOnly bool   `json:"readOnly"`
}

type HistoryEntry struct {
	ConnID   string    `json:"connId"`
	Database string    `json:"database,omitempty"`
	Query    string    `json:"query"`
	Source   string    `json:"source"` // "ui" or "mcp"
	At       time.Time `json:"at"`
	Millis   int64     `json:"ms"`
	Error    string    `json:"error,omitempty"`
}

var ErrNotFound = errors.New("connection not found")

type Store struct {
	dir string
	mu  sync.Mutex
}

// DefaultDir is $SLIMDB_HOME, or <user config dir>/slimdb.
func DefaultDir() (string, error) {
	if v := os.Getenv("SLIMDB_HOME"); v != "" {
		return v, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "slimdb"), nil
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) connPath() string    { return filepath.Join(s.dir, "connections.json") }
func (s *Store) historyPath() string { return filepath.Join(s.dir, "history.jsonl") }

// KindFromURL guesses the connection kind from the URL scheme.
func KindFromURL(url string) (string, error) {
	switch {
	case strings.HasPrefix(url, "postgres://"), strings.HasPrefix(url, "postgresql://"):
		return KindPostgres, nil
	case strings.HasPrefix(url, "mongodb://"), strings.HasPrefix(url, "mongodb+srv://"):
		return KindMongo, nil
	}
	return "", fmt.Errorf("unsupported URL scheme, use postgres:// or mongodb://")
}

func (s *Store) Conns() ([]Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *Store) Conn(idOrName string) (Conn, error) {
	conns, err := s.Conns()
	if err != nil {
		return Conn{}, err
	}
	for _, c := range conns {
		if c.ID == idOrName || strings.EqualFold(c.Name, idOrName) {
			return c, nil
		}
	}
	return Conn{}, ErrNotFound
}

// Save creates the connection when c.ID is empty and replaces it otherwise.
func (s *Store) Save(c Conn) (Conn, error) {
	c.Name = strings.TrimSpace(c.Name)
	c.URL = strings.TrimSpace(c.URL)
	if c.Name == "" || c.URL == "" {
		return c, errors.New("name and url are required")
	}
	kind, err := KindFromURL(c.URL)
	if err != nil {
		return c, err
	}
	c.Kind = kind

	s.mu.Lock()
	defer s.mu.Unlock()
	conns, err := s.load()
	if err != nil {
		return c, err
	}
	for _, other := range conns {
		if other.ID != c.ID && strings.EqualFold(other.Name, c.Name) {
			return c, fmt.Errorf("a connection named %q already exists", c.Name)
		}
	}
	if c.ID == "" {
		c.ID = newID()
		conns = append(conns, c)
	} else {
		i := indexOf(conns, c.ID)
		if i < 0 {
			return c, ErrNotFound
		}
		conns[i] = c
	}
	return c, s.write(conns)
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	conns, err := s.load()
	if err != nil {
		return err
	}
	i := indexOf(conns, id)
	if i < 0 {
		return ErrNotFound
	}
	return s.write(append(conns[:i], conns[i+1:]...))
}

func (s *Store) load() ([]Conn, error) {
	b, err := os.ReadFile(s.connPath())
	if errors.Is(err, os.ErrNotExist) {
		return []Conn{}, nil
	}
	if err != nil {
		return nil, err
	}
	var conns []Conn
	if err := json.Unmarshal(b, &conns); err != nil {
		return nil, fmt.Errorf("read %s: %w", s.connPath(), err)
	}
	return conns, nil
}

// write replaces the file atomically. The file holds passwords, so it is 0600.
func (s *Store) write(conns []Conn) error {
	b, err := json.MarshalIndent(conns, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.connPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.connPath())
}

// AddHistory appends one entry. Failures are ignored: history is a convenience.
func (s *Store) AddHistory(e HistoryEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(s.historyPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()

	if entries, err := s.readHistory(); err == nil && len(entries) > historyTrim {
		s.rewriteHistory(entries[len(entries)-historyKeep:])
	}
}

// History returns the newest entries first, optionally for one connection.
func (s *Store) History(connID string, limit int) ([]HistoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.readHistory()
	if err != nil {
		return nil, err
	}
	out := []HistoryEntry{}
	for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
		if connID == "" || entries[i].ConnID == connID {
			out = append(out, entries[i])
		}
	}
	return out, nil
}

func (s *Store) readHistory() ([]HistoryEntry, error) {
	f, err := os.Open(s.historyPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []HistoryEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var e HistoryEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

func (s *Store) rewriteHistory(entries []HistoryEntry) {
	var sb strings.Builder
	for _, e := range entries {
		b, _ := json.Marshal(e)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	tmp := s.historyPath() + ".tmp"
	if os.WriteFile(tmp, []byte(sb.String()), 0o600) == nil {
		_ = os.Rename(tmp, s.historyPath())
	}
}

func indexOf(conns []Conn, id string) int {
	for i, c := range conns {
		if c.ID == id {
			return i
		}
	}
	return -1
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
