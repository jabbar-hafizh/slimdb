package db

// Integration tests against real databases. They are skipped unless
// SLIMDB_TEST_POSTGRES_URL and/or SLIMDB_TEST_MONGO_URL are set, e.g.
//
//	SLIMDB_TEST_POSTGRES_URL=postgres://postgres:pass@localhost:5432/postgres \
//	SLIMDB_TEST_MONGO_URL=mongodb://localhost:27017 go test ./...

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func testURL(t *testing.T, env string) string {
	t.Helper()
	url := os.Getenv(env)
	if url == "" {
		t.Skipf("%s not set", env)
	}
	return url
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustQuery(t *testing.T, d Driver, database, q string, limit int) []Result {
	t.Helper()
	res, err := d.Query(ctxT(t), database, q, limit)
	if err != nil {
		t.Fatalf("Query(%q): %v", q, err)
	}
	for _, r := range res {
		if r.Error != "" {
			t.Fatalf("Query(%q): statement %q failed: %s", q, r.Statement, r.Error)
		}
	}
	return res
}

func TestPostgres(t *testing.T) {
	url := testURL(t, "SLIMDB_TEST_POSTGRES_URL")
	pg, err := openPostgres(ctxT(t), url, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)

	mustQuery(t, pg, "", `
		drop schema if exists slimdb_test cascade;
		create schema slimdb_test;
		create table slimdb_test.users (
			id serial primary key,
			name text not null,
			age int,
			tags jsonb
		);
		create index users_name_idx on slimdb_test.users (name);
		create view slimdb_test.adults as select * from slimdb_test.users where age >= 18;
		insert into slimdb_test.users (name, age, tags) values
			('ann', 31, '["a"]'), ('bob', null, null), ('cy', 12, '{"k": 1}');`, 0)
	t.Cleanup(func() { _, _ = pg.Query(context.Background(), "", "drop schema slimdb_test cascade", 0) })

	t.Run("multiple statements share a session", func(t *testing.T) {
		res := mustQuery(t, pg, "", "insert into slimdb_test.users (name) values ('dee'); select name, age from slimdb_test.users order by id", 100)
		if len(res) != 2 || res[0].Affected != 1 {
			t.Fatalf("results = %+v", res)
		}
		r := res[1]
		if len(r.Rows) != 4 || r.Columns[0].Name != "name" || r.Columns[1].Type != "int4" {
			t.Fatalf("select result = %+v", r)
		}
		if r.Rows[1][1] != nil {
			t.Errorf("NULL age came back as %#v", r.Rows[1][1])
		}
		if r.Rows[0][1] != "31" {
			t.Errorf("age = %#v, want the text \"31\"", r.Rows[0][1])
		}
	})

	t.Run("limit marks truncation", func(t *testing.T) {
		r := mustQuery(t, pg, "", "select * from generate_series(1, 10)", 3)[0]
		if len(r.Rows) != 3 || !r.Truncated {
			t.Fatalf("rows=%d truncated=%v", len(r.Rows), r.Truncated)
		}
		r = mustQuery(t, pg, "", "select * from generate_series(1, 3)", 3)[0]
		if len(r.Rows) != 3 || r.Truncated {
			t.Fatalf("exact limit: rows=%d truncated=%v", len(r.Rows), r.Truncated)
		}
	})

	t.Run("data-modifying WITH falls back from the cursor", func(t *testing.T) {
		r := mustQuery(t, pg, "", "with x as (insert into slimdb_test.users (name) values ('eve') returning name) select * from x", 10)[0]
		if len(r.Rows) != 1 || r.Rows[0][0] != "eve" {
			t.Fatalf("result = %+v", r)
		}
	})

	t.Run("stops at the first error", func(t *testing.T) {
		res, err := pg.Query(ctxT(t), "", "select 1; select nope; select 2", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 2 || res[1].Error == "" || res[0].Error != "" {
			t.Fatalf("results = %+v", res)
		}
	})

	t.Run("explicit transaction with a select inside", func(t *testing.T) {
		res := mustQuery(t, pg, "", `
			begin;
			delete from slimdb_test.users;
			select count(*) from slimdb_test.users;
			rollback;
			select count(*) from slimdb_test.users;`, 10)
		if got := res[2].Rows[0][0]; got != "0" {
			t.Errorf("count inside tx = %v, want 0", got)
		}
		if got := res[4].Rows[0][0]; got == "0" {
			t.Errorf("rollback did not restore rows")
		}
	})

	t.Run("tree lists tables and views", func(t *testing.T) {
		nodes, err := pg.Tree(ctxT(t))
		if err != nil {
			t.Fatal(err)
		}
		var found []string
		for _, n := range nodes {
			if n.Name == "slimdb_test" {
				for _, c := range n.Children {
					found = append(found, c.Kind+":"+c.Object)
				}
			}
		}
		if strings.Join(found, ",") != "view:slimdb_test.adults,table:slimdb_test.users" {
			t.Errorf("found %v", found)
		}
	})

	t.Run("describe", func(t *testing.T) {
		res, err := pg.Describe(ctxT(t), "", "slimdb_test.users")
		if err != nil {
			t.Fatal(err)
		}
		cols := res[0]
		if cols.Rows[0][0] != "id" || cols.Rows[0][4] != "t" || cols.Rows[1][2] != "f" {
			t.Errorf("columns = %v", cols.Rows)
		}
		if len(res[1].Rows) != 2 {
			t.Errorf("indexes = %v", res[1].Rows)
		}
		if _, err := pg.Describe(ctxT(t), "", "slimdb_test.nope"); err == nil {
			t.Error("describe of a missing table succeeded")
		}
	})

	t.Run("explain analyze rolls back", func(t *testing.T) {
		res, err := pg.Explain(ctxT(t), "", "delete from slimdb_test.users", true)
		if err != nil || res[0].Error != "" || len(res[0].Rows) == 0 {
			t.Fatalf("explain: %v %+v", err, res)
		}
		r := mustQuery(t, pg, "", "select count(*) from slimdb_test.users", 1)[0]
		if r.Rows[0][0] == "0" {
			t.Error("explain analyze deleted the rows")
		}
	})

	t.Run("read-only connection refuses writes", func(t *testing.T) {
		ro, err := openPostgres(ctxT(t), url, true)
		if err != nil {
			t.Fatal(err)
		}
		defer ro.Close()
		res, err := ro.Query(ctxT(t), "", "select count(*) from slimdb_test.users; delete from slimdb_test.users", 10)
		if err != nil {
			t.Fatal(err)
		}
		if res[0].Error != "" || res[1].Error == "" {
			t.Fatalf("results = %+v", res)
		}
	})
}

func TestMongo(t *testing.T) {
	url := testURL(t, "SLIMDB_TEST_MONGO_URL")
	m, err := openMongo(ctxT(t), url, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	const dbName = "slimdb_test"
	_ = m.client.Database(dbName).Drop(ctxT(t))
	t.Cleanup(func() { _ = m.client.Database(dbName).Drop(context.Background()) })

	mustQuery(t, m, dbName, `
		db.users.insertMany([
			{ name: "ann", age: 31, tags: ["a", "b"], at: ISODate("2026-01-01T00:00:00Z") },
			{ name: "bob", age: 25 },
			{ name: "cy", age: 12, address: { city: "Jakarta" } },
		])
		db.users.createIndex({ name: 1 }, { unique: true, name: "name_unique" })`, 0)

	t.Run("find with modifiers", func(t *testing.T) {
		r := mustQuery(t, m, dbName, `db.users.find({ age: { $gte: 18 } }, { _id: 0 }).sort({ age: -1 })`, 10)[0]
		if len(r.Rows) != 2 || r.Columns[0].Name != "name" || r.Rows[0][0] != "ann" {
			t.Fatalf("result = %+v", r)
		}
		if len(r.Docs) != 2 || !strings.Contains(r.Docs[0], `"tags":["a","b"]`) {
			t.Errorf("docs = %v", r.Docs)
		}
	})

	t.Run("limit marks truncation, user limit does not", func(t *testing.T) {
		r := mustQuery(t, m, dbName, `db.users.find()`, 2)[0]
		if len(r.Rows) != 2 || !r.Truncated {
			t.Fatalf("rows=%d truncated=%v", len(r.Rows), r.Truncated)
		}
		r = mustQuery(t, m, dbName, `db.users.find().limit(2)`, 100)[0]
		if len(r.Rows) != 2 || r.Truncated {
			t.Fatalf("user limit: rows=%d truncated=%v", len(r.Rows), r.Truncated)
		}
	})

	t.Run("aggregate, count, distinct", func(t *testing.T) {
		res := mustQuery(t, m, dbName, `
			db.users.aggregate([{ $group: { _id: null, total: { $sum: "$age" } } }])
			db.users.countDocuments({ age: { $lt: 30 } })
			db.users.distinct("name")`, 10)
		if res[0].Rows[0][1] != int32(68) && res[0].Rows[0][1] != int64(68) {
			t.Errorf("sum = %#v", res[0].Rows[0][1])
		}
		if res[1].Rows[0][0] != int64(2) {
			t.Errorf("count = %#v", res[1].Rows[0][0])
		}
		if len(res[2].Rows) != 3 {
			t.Errorf("distinct = %v", res[2].Rows)
		}
	})

	t.Run("update and delete", func(t *testing.T) {
		res := mustQuery(t, m, dbName, `
			db.users.updateOne({ name: "bob" }, { $set: { age: 26 } })
			db.users.updateOne({ name: "zed" }, { $set: { age: 1 } }, { upsert: true })
			db.users.deleteMany({ name: "zed" })`, 10)
		if res[0].Affected != 1 || res[1].Affected != 1 || res[2].Affected != 1 {
			t.Fatalf("results = %+v", res)
		}
	})

	t.Run("use, show, and errors stop the run", func(t *testing.T) {
		res, err := m.Query(ctxT(t), "", "use "+dbName+"\nshow collections\ndb.users.badMethod()\ndb.users.find()", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 3 || res[1].Rows[0][0] != "users" || res[2].Error == "" {
			t.Fatalf("results = %+v", res)
		}
	})

	t.Run("syntax errors come back as a result", func(t *testing.T) {
		res, err := m.Query(ctxT(t), dbName, "db.users.find({a: })", 10)
		if err != nil || len(res) != 1 || !strings.Contains(res[0].Error, "line 1") {
			t.Fatalf("err=%v results=%+v", err, res)
		}
	})

	t.Run("tree and describe", func(t *testing.T) {
		nodes, err := m.Tree(ctxT(t))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, n := range nodes {
			if n.Name == dbName && len(n.Children) == 1 && n.Children[0].Name == "users" {
				found = true
			}
		}
		if !found {
			t.Errorf("tree = %+v", nodes)
		}
		res, err := m.Describe(ctxT(t), dbName, "users")
		if err != nil {
			t.Fatal(err)
		}
		if len(res[0].Rows) < 5 || len(res[1].Rows) != 2 {
			t.Errorf("fields=%v indexes=%v", res[0].Rows, res[1].Rows)
		}
	})

	t.Run("explain", func(t *testing.T) {
		res, err := m.Explain(ctxT(t), dbName, `db.users.find({ name: "ann" })`, true)
		if err != nil || res[0].Error != "" || !strings.Contains(res[0].Docs[0], "name_unique") {
			t.Fatalf("err=%v res=%+v", err, res)
		}
	})

	t.Run("read-only connection refuses writes", func(t *testing.T) {
		ro, err := openMongo(ctxT(t), url, true)
		if err != nil {
			t.Fatal(err)
		}
		defer ro.Close()
		for _, q := range []string{
			`db.users.deleteMany({})`,
			`db.users.aggregate([{ $match: {} }, { $out: "copy" }])`,
			`db.runCommand({ drop: "users" })`,
		} {
			res, err := ro.Query(ctxT(t), dbName, q, 10)
			if err != nil || !strings.Contains(res[0].Error, "read-only") {
				t.Errorf("%s: err=%v res=%+v", q, err, res)
			}
		}
		mustQuery(t, ro, dbName, `db.users.find()`, 10)
	})
}
