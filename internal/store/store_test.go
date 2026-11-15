package store

import (
	"errors"
	"testing"
	"time"
)

func TestConnLifecycle(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Save(Conn{Name: "local", URL: "postgres://u:p@localhost/app"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == "" || c.Kind != KindPostgres {
		t.Fatalf("saved = %+v", c)
	}
	if _, err := s.Save(Conn{Name: "LOCAL", URL: "mongodb://localhost"}); err == nil {
		t.Error("duplicate name (case-insensitive) was accepted")
	}
	if _, err := s.Save(Conn{Name: "x", URL: "oracle://localhost"}); err == nil {
		t.Error("unsupported scheme was accepted")
	}

	c.URL = "mongodb://localhost:27017/app"
	if _, err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	got, err := s.Conn("Local")
	if err != nil || got.Kind != KindMongo {
		t.Fatalf("Conn by name = %+v, %v", got, err)
	}
	if err := s.Delete(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Conn(c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
}

func TestHistoryNewestFirstAndTrimmed(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range historyTrim + 1 {
		conn := "a"
		if i%2 == 1 {
			conn = "b"
		}
		s.AddHistory(HistoryEntry{ConnID: conn, Query: string(rune('a' + i%26)), At: time.Now()})
	}
	all, err := s.readHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != historyKeep {
		t.Errorf("kept %d entries, want %d", len(all), historyKeep)
	}
	onlyB, err := s.History("b", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyB) != 3 || onlyB[0].ConnID != "b" {
		t.Errorf("History(b) = %+v", onlyB)
	}
}

func TestKindFromURL(t *testing.T) {
	for url, want := range map[string]string{
		"postgresql://h/db": KindPostgres, "mysql://h/db": KindMySQL, "mariadb://h/db": KindMySQL,
		"sqlite:///tmp/a.db": KindSQLite, "file:a.db": KindSQLite, "sqlserver://h": KindSQLServer,
		"mongodb+srv://h": KindMongo, "redis://h": KindRedis, "rediss://h": KindRedis,
	} {
		if got, err := KindFromURL(url); err != nil || got != want {
			t.Errorf("KindFromURL(%q) = %q, %v", url, got, err)
		}
	}
}
