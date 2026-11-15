package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver, pure Go
)

type sqliteEngine struct{}

// sqlitePath takes sqlite:///abs/path.db, sqlite://relative.db,
// sqlite:path.db or a file: URI and returns the file path and any query.
func sqlitePath(raw string) (path, query string) {
	s := raw
	if strings.HasPrefix(s, "file:") {
		s = strings.TrimPrefix(s, "file:")
	} else {
		s = strings.TrimPrefix(s, "sqlite:")
	}
	s = strings.TrimPrefix(s, "//")
	if i := strings.Index(s, "?"); i >= 0 {
		s, query = s[:i], s[i+1:]
	}
	if p, err := url.PathUnescape(s); err == nil {
		s = p
	}
	return s, query
}

// openSQLite opens a database file. The file must exist, so a typo in the
// path does not quietly create an empty database. readOnly opens it with
// mode=ro, which SQLite itself enforces.
func openSQLite(ctx context.Context, raw string, readOnly bool) (*sqlDB, error) {
	path, query := sqlitePath(raw)
	if path == "" {
		return nil, errors.New("sqlite URL needs a file path, e.g. sqlite:///home/me/app.db")
	}
	memory := path == ":memory:"
	if !memory {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("sqlite file %s: %w (create it first, e.g. with sqlite3 %s \"vacuum\")", path, err, path)
		}
	}
	params, err := url.ParseQuery(query)
	if err != nil {
		return nil, err
	}
	if readOnly {
		params.Set("mode", "ro")
	}
	params.Add("_pragma", "busy_timeout(5000)")
	params.Add("_pragma", "foreign_keys(1)")
	dsn := "file:" + path + "?" + params.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if memory {
		db.SetMaxOpenConns(1) // every connection would get its own empty database
	} else {
		db.SetMaxOpenConns(4)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return &sqlDB{db: db, eng: sqliteEngine{}, readOnly: readOnly}, nil
}

func (sqliteEngine) dialect() sqlDialect { return dialectSQLite }

func (sqliteEngine) split(script string) []string {
	return splitSQL(script, splitOpts{mysql: false, brackets: true})
}

func (sqliteEngine) schemas(ctx context.Context, q sqlQueryer) ([]string, error) {
	return queryStrings(ctx, q, "select name from pragma_database_list order by seq")
}

func (e sqliteEngine) tree(ctx context.Context, db *sql.DB) ([]Node, error) {
	schemas, err := e.schemas(ctx, db)
	if err != nil {
		return nil, err
	}
	qt := dialectSQLite.quote
	var out []treeRow
	for _, s := range schemas {
		rows, err := db.QueryContext(ctx, `select name, type from `+qt(s)+`.sqlite_master
			where type in ('table', 'view') and name not like 'sqlite\_%' escape '\' order by name`)
		if err != nil {
			return nil, err
		}
		n := 0
		for rows.Next() {
			var name, typ string
			if err := rows.Scan(&name, &typ); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, treeRow{group: s, name: name, kind: typ, object: qt(s) + "." + qt(name)})
			n++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if n == 0 && s == "main" {
			out = append(out, treeRow{group: s})
		}
	}
	return groupTree(out, "schema"), nil
}

func (e sqliteEngine) resolve(ctx context.Context, q sqlQueryer, ref string) (tableRef, error) {
	parts, err := splitQualified(ref)
	if err != nil {
		return tableRef{}, err
	}
	var schemas []string
	var name string
	switch len(parts) {
	case 1:
		if schemas, err = e.schemas(ctx, q); err != nil {
			return tableRef{}, err
		}
		name = parts[0]
	case 2:
		schemas, name = []string{parts[0]}, parts[1]
	default:
		return tableRef{}, fmt.Errorf("bad table name %q", ref)
	}
	qt := dialectSQLite.quote
	for _, s := range schemas {
		var real, typ string
		err := q.QueryRowContext(ctx, `select name, type from `+qt(s)+`.sqlite_master
			where type in ('table', 'view') and name = ? collate nocase`, name).Scan(&real, &typ)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return tableRef{}, err
		}
		t := tableRef{schema: s, name: real, isTable: typ == "table", object: qt(s) + "." + qt(real)}
		if t.cols, err = queryStrings(ctx, q, `select name from pragma_table_info(?, ?) order by cid`, real, s); err != nil {
			return t, err
		}
		t.pk, err = queryStrings(ctx, q, `select name from pragma_table_info(?, ?) where pk > 0 order by pk`, real, s)
		return t, err
	}
	return tableRef{}, fmt.Errorf("table %s not found", ref)
}

func (sqliteEngine) describe(ctx context.Context, q sqlQueryer, t tableRef) ([]Result, error) {
	sections := []struct {
		title, sql string
		args       []any
	}{
		{"Columns", `select name, type,
				case when "notnull" then 'false' else 'true' end as nullable,
				dflt_value as "default", case when pk > 0 then 'true' else 'false' end as primary_key
			from pragma_table_info(?, ?) order by cid`, []any{t.name, t.schema}},
		{"Indexes", `select name, sql as definition from ` + dialectSQLite.quote(t.schema) + `.sqlite_master
			where type = 'index' and tbl_name = ? order by name`, []any{t.name}},
		{"Foreign keys", `select id, "from" as columns, "table" || ' (' || "to" || ')' as "references",
				on_update, on_delete
			from pragma_foreign_key_list(?, ?) order by id, seq`, []any{t.name, t.schema}},
	}
	var out []Result
	for _, s := range sections {
		r, err := queryResult(ctx, q, s.title, s.sql, s.args...)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// explain uses EXPLAIN QUERY PLAN. SQLite has no analyze variant that
// keeps the plan readable, so analyze is ignored.
func (sqliteEngine) explain(ctx context.Context, conn *sql.Conn, stmt string, _ bool) Result {
	var r Result
	rows, err := conn.QueryContext(ctx, "explain query plan "+stmt)
	if err == nil {
		err = readSQLRows(rows, 0, &r)
	}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}
