package db

// Integration tests for the database/sql drivers and Redis. Each is skipped
// unless its URL is set, e.g.
//
//	SLIMDB_TEST_MYSQL_URL=mysql://root:pass@localhost:3306/shop
//	SLIMDB_TEST_SQLITE=1   (uses a temporary file)
//	SLIMDB_TEST_SQLSERVER_URL=sqlserver://sa:Pass_word1@localhost:1433
//	SLIMDB_TEST_REDIS_URL=redis://localhost:6379/0

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type sqlCase struct {
	env    string
	open   func(ctx context.Context, url string, readOnly bool) (*sqlDB, error)
	url    func(t *testing.T) string
	setup  string
	object string // canonical reference of the test table
	group  string // tree group holding it
}

func sqlCases() map[string]sqlCase {
	common := `create table slimdb_items (id int primary key, name varchar(50), qty int, note varchar(100));
		create view slimdb_v as select id, name from slimdb_items;
		insert into slimdb_items values (1, 'ann', 3, 'it''s'), (2, 'bob', null, null), (3, 'cy', 7, 'x;y')`
	return map[string]sqlCase{
		"mysql": {
			env: "SLIMDB_TEST_MYSQL_URL", open: openMySQL,
			setup:  "drop view if exists slimdb_v; drop table if exists slimdb_items; " + common,
			object: "`shop`.`slimdb_items`", group: "shop",
		},
		"sqlite": {
			env: "SLIMDB_TEST_SQLITE", open: openSQLite,
			url: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "test.db")
				if err := os.WriteFile(p, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return "sqlite://" + p
			},
			setup:  common,
			object: `"main"."slimdb_items"`, group: "main",
		},
		"sqlserver": {
			env: "SLIMDB_TEST_SQLSERVER_URL", open: openSQLServer,
			setup:  "drop view if exists slimdb_v; drop table if exists slimdb_items; " + common,
			object: "[dbo].[slimdb_items]", group: "dbo",
		},
	}
}

func TestSQLDrivers(t *testing.T) {
	for name, c := range sqlCases() {
		t.Run(name, func(t *testing.T) {
			url := testURL(t, c.env)
			if c.url != nil {
				url = c.url(t)
			}
			d, err := c.open(ctxT(t), url, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(d.Close)
			mustQuery(t, d, "", c.setup, 0)

			t.Run("select, limit and edit info", func(t *testing.T) {
				r := mustQuery(t, d, "", "select * from slimdb_items order by id", 2)[0]
				if len(r.Rows) != 2 || !r.Truncated || r.Columns[0].Name != "id" {
					t.Fatalf("result = %+v", r)
				}
				if r.Rows[0][3] != "it's" || r.Rows[1][2] != nil {
					t.Errorf("values = %#v", r.Rows)
				}
				if r.Edit == nil || r.Edit.Object != c.object || len(r.Edit.Key) != 1 || r.Edit.Key[0] != "id" {
					t.Fatalf("edit = %+v", r.Edit)
				}
				if v := mustQuery(t, d, "", "select * from slimdb_v", 0)[0]; v.Edit != nil {
					t.Errorf("a view must not be editable: %+v", v.Edit)
				}
				if v := mustQuery(t, d, "", "select count(*) from slimdb_items", 0)[0]; v.Edit != nil || v.Rows[0][0] != "3" {
					t.Errorf("count = %+v", v)
				}
			})

			t.Run("statements run in order and stop at an error", func(t *testing.T) {
				res, err := d.Query(ctxT(t), "", "update slimdb_items set qty = qty where id = 1; select nope from nowhere; select 1", 0)
				if err != nil {
					t.Fatal(err)
				}
				if len(res) != 2 || res[0].Affected != 1 || res[1].Error == "" {
					t.Fatalf("results = %+v", res)
				}
			})

			t.Run("tree and describe", func(t *testing.T) {
				nodes, err := d.Tree(ctxT(t))
				if err != nil {
					t.Fatal(err)
				}
				var kinds []string
				for _, g := range nodes {
					if g.Name != c.group {
						continue
					}
					for _, ch := range g.Children {
						if strings.HasPrefix(ch.Name, "slimdb_") {
							kinds = append(kinds, ch.Kind+":"+ch.Object)
						}
					}
				}
				if len(kinds) != 2 || kinds[0] != "table:"+c.object {
					t.Fatalf("tree children = %v (groups %+v)", kinds, nodes)
				}
				res, err := d.Describe(ctxT(t), "", c.object)
				if err != nil {
					t.Fatal(err)
				}
				cols := res[0]
				if cols.Title != "Columns" || len(cols.Rows) != 4 || cols.Rows[0][0] != "id" || cols.Rows[0][4] != "true" {
					t.Fatalf("columns = %+v", cols)
				}
				if _, err := d.Describe(ctxT(t), "", "slimdb_items"); err != nil {
					t.Errorf("describe without schema: %v", err)
				}
			})

			t.Run("explain", func(t *testing.T) {
				for _, analyze := range []bool{false, true} {
					res, err := d.Explain(ctxT(t), "", "select * from slimdb_items where id = 1", analyze)
					if err != nil || res[0].Error != "" || len(res[0].Rows) == 0 {
						t.Fatalf("explain analyze=%v: %v %+v", analyze, err, res)
					}
				}
			})

			t.Run("apply edits", func(t *testing.T) {
				r, err := d.Apply(ctxT(t), "", Changes{Object: c.object, Ops: []EditOp{
					{Op: "update", Key: map[string]any{"id": "1"}, Values: map[string]any{"name": "anne", "qty": "3"}},
					{Op: "insert", Values: map[string]any{"id": "10", "name": "new", "qty": nil}},
					{Op: "delete", Key: map[string]any{"id": "2"}},
				}})
				if err != nil || r.Affected != 3 {
					t.Fatalf("apply: %v %+v", err, r)
				}
				got := mustQuery(t, d, "", "select id, name from slimdb_items order by id", 0)[0].Rows
				if len(got) != 3 || got[0][1] != "anne" || got[2][0] != "10" {
					t.Fatalf("rows after apply = %v", got)
				}
				// A key that matches nothing undoes the whole batch.
				_, err = d.Apply(ctxT(t), "", Changes{Object: c.object, Ops: []EditOp{
					{Op: "insert", Values: map[string]any{"id": "20"}},
					{Op: "update", Key: map[string]any{"id": "999"}, Values: map[string]any{"name": "x"}},
				}})
				if err == nil || !strings.Contains(err.Error(), "matched 0 rows") {
					t.Fatalf("want a 0-rows error, got %v", err)
				}
				if n := mustQuery(t, d, "", "select count(*) from slimdb_items where id = 20", 0)[0].Rows[0][0]; n != "0" {
					t.Errorf("insert was not rolled back")
				}
				if _, err := d.Apply(ctxT(t), "", Changes{Object: "slimdb_v", Ops: []EditOp{{Op: "delete", Key: map[string]any{"id": "1"}}}}); err == nil {
					t.Error("editing a view must fail")
				}
				if _, err := d.Apply(ctxT(t), "", Changes{Object: "nope; drop table slimdb_items", Ops: []EditOp{{Op: "delete", Key: map[string]any{"id": "1"}}}}); err == nil {
					t.Error("a made-up object must fail")
				}
			})

			t.Run("read-only connection", func(t *testing.T) {
				ro, err := c.open(ctxT(t), url, true)
				if err != nil {
					t.Fatal(err)
				}
				defer ro.Close()
				mustQuery(t, ro, "", "select * from slimdb_items", 0)
				res, err := ro.Query(ctxT(t), "", "delete from slimdb_items", 0)
				if err != nil || res[0].Error == "" {
					t.Fatalf("delete on read-only: %v %+v", err, res)
				}
				if _, err := ro.Apply(ctxT(t), "", Changes{Object: c.object, Ops: []EditOp{{Op: "delete", Key: map[string]any{"id": "1"}}}}); !errors.Is(err, ErrReadOnly) {
					t.Errorf("apply on read-only: %v", err)
				}
			})
		})
	}
}

func TestMySQLServerSideReadOnly(t *testing.T) {
	url := testURL(t, "SLIMDB_TEST_MYSQL_URL")
	ro, err := openMySQL(ctxT(t), url, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	// A write hidden behind a read keyword still fails on the server.
	conn, err := ro.db.Conn(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctxT(t), "insert into slimdb_items (id) values (99)"); err == nil {
		t.Fatal("server accepted a write on a read-only session")
	}
}

func TestRedis(t *testing.T) {
	url := testURL(t, "SLIMDB_TEST_REDIS_URL")
	r, err := openRedis(ctxT(t), url, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	mustQuery(t, r, "1", `flushdb
		set greeting "hello world"
		hset user:1 name ann city 'Bandung'
		rpush queue a b c
		zadd board 10 ann 5 bob
		sadd tags x y
		xadd events * kind login`, 0)

	t.Run("replies as tables", func(t *testing.T) {
		res := mustQuery(t, r, "1", "get greeting\nhgetall user:1\nlrange queue 0 -1\nget missing\nzrange board 0 -1 withscores\nscan 0\ninfo server", 2)
		if res[0].Rows[0][0] != "hello world" {
			t.Errorf("get = %+v", res[0])
		}
		if res[1].Columns[0].Name != "field" || len(res[1].Rows) != 2 || res[1].Rows[0][0] != "city" {
			t.Errorf("hgetall = %+v", res[1])
		}
		if len(res[2].Rows) != 2 || !res[2].Truncated {
			t.Errorf("lrange with limit = %+v", res[2])
		}
		if res[3].Rows[0][0] != nil {
			t.Errorf("missing key = %+v", res[3])
		}
		if len(res[4].Rows) != 2 {
			t.Errorf("zrange withscores = %+v", res[4])
		}
		if res[5].Message == "" {
			t.Errorf("scan = %+v", res[5])
		}
		if res[6].Columns[0].Name != "key" {
			t.Errorf("info = %+v", res[6])
		}
		bad, _ := r.Query(ctxT(t), "1", "nosuchcommand\nget greeting", 0)
		if len(bad) != 1 || bad[0].Error == "" {
			t.Errorf("unknown command = %+v", bad)
		}
	})

	t.Run("tree and describe", func(t *testing.T) {
		nodes, err := r.Tree(ctxT(t))
		if err != nil {
			t.Fatal(err)
		}
		var db1 *Node
		for i := range nodes {
			if nodes[i].Name == "db1" {
				db1 = &nodes[i]
			}
		}
		if db1 == nil || len(db1.Children) != 6 {
			t.Fatalf("tree = %+v", nodes)
		}
		kinds := map[string]string{}
		for _, k := range db1.Children {
			kinds[k.Name] = k.Kind
			if k.Database != "1" || k.Object != k.Name {
				t.Errorf("node = %+v", k)
			}
		}
		if kinds["user:1"] != "hash" || kinds["board"] != "zset" || kinds["events"] != "stream" {
			t.Errorf("kinds = %v", kinds)
		}
		for key, rows := range map[string]int{"greeting": 1, "user:1": 2, "queue": 3, "board": 2, "tags": 2, "events": 1} {
			res, err := r.Describe(ctxT(t), "1", key)
			if err != nil || len(res) != 2 || len(res[1].Rows) != rows {
				t.Errorf("describe %s: %v %+v", key, err, res)
			}
		}
	})

	t.Run("read-only refuses writes", func(t *testing.T) {
		ro, err := openRedis(ctxT(t), url, true)
		if err != nil {
			t.Fatal(err)
		}
		defer ro.Close()
		mustQuery(t, ro, "1", "get greeting", 0)
		res, _ := ro.Query(ctxT(t), "1", "del greeting", 0)
		if res[0].Error == "" {
			t.Fatal("del ran on a read-only connection")
		}
	})
	_, _ = r.Query(context.Background(), "1", "flushdb", 0)
}

func TestSplitCommand(t *testing.T) {
	got, err := splitCommand(`set "a b" 'it\'s' "line\nnext" plain`)
	if err != nil || len(got) != 5 || got[1] != "a b" || got[2] != "it's" || got[3] != "line\nnext" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := splitCommand(`get "open`); err == nil {
		t.Fatal("unbalanced quote must fail")
	}
}

func TestSplitQualified(t *testing.T) {
	for in, want := range map[string]string{
		"shop.users":       "shop|users",
		"`shop`.`us``ers`": "shop|us`ers",
		"[dbo].[Or]]ders]": "dbo|Or]ders",
		`main."my table"`:  "main|my table",
		"users":            "users",
	} {
		got, err := splitQualified(in)
		if err != nil || strings.Join(got, "|") != want {
			t.Errorf("splitQualified(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := splitQualified("a.b c"); err == nil {
		t.Error("junk after a name must fail")
	}
}
