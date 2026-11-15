package db

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// sqlDB is the driver for SQL databases reached through database/sql
// (MySQL/MariaDB, SQLite, SQL Server). What differs between them lives in a
// sqlEngine.
type sqlDB struct {
	db       *sql.DB
	eng      sqlEngine
	readOnly bool
}

// sqlEngine holds the parts of a SQL driver that depend on the database.
type sqlEngine interface {
	tree(ctx context.Context, db *sql.DB) ([]Node, error)
	describe(ctx context.Context, q sqlQueryer, ref tableRef) ([]Result, error)
	explain(ctx context.Context, conn *sql.Conn, stmt string, analyze bool) Result
	// resolve finds the table or view a user named, quoted or not, and
	// returns its canonical quoted reference with its columns and key.
	resolve(ctx context.Context, q sqlQueryer, ref string) (tableRef, error)
	dialect() sqlDialect
	// split turns a script into statements.
	split(script string) []string
}

type tableRef struct {
	object  string // canonical quoted reference, e.g. `shop`.`users`
	schema  string
	name    string
	isTable bool
	cols    []string
	pk      []string
}

// sqlQueryer is what *sql.DB, *sql.Conn and *sql.Tx have in common.
type sqlQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var errReadOnlyStmt = errors.New("this connection is read-only")

func (s *sqlDB) Close() { _ = s.db.Close() }

func (s *sqlDB) Tree(ctx context.Context) ([]Node, error) { return s.eng.tree(ctx, s.db) }

func (s *sqlDB) Describe(ctx context.Context, _ string, object string) ([]Result, error) {
	ref, err := s.eng.resolve(ctx, s.db, object)
	if err != nil {
		return nil, err
	}
	return s.eng.describe(ctx, s.db, ref)
}

// Query runs the statements on one connection, so session state such as
// USE, SET or temp tables carries across them.
func (s *sqlDB) Query(ctx context.Context, _ string, q string, limit int) ([]Result, error) {
	stmts := s.eng.split(q)
	if len(stmts) == 0 {
		return nil, errors.New("nothing to run")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	var out []Result
	for i, stmt := range stmts {
		r := s.run(ctx, conn, stmt, limit, i == len(stmts)-1)
		out = append(out, r)
		if r.Error != "" {
			break
		}
	}
	return out, nil
}

// rowKeywords start statements that return rows.
var rowKeywords = []string{"select", "with", "show", "describe", "desc", "explain", "values", "table",
	"pragma", "exec", "execute", "call", "help", "analyze", "check", "checksum", "optimize", "repair"}

// readKeywords may run on a read-only connection. The server-side guards
// (MySQL's read-only session, SQLite's read-only mode) back this up.
var readKeywords = []string{"select", "with", "show", "describe", "desc", "explain", "values", "table", "pragma", "use", "help"}

// run runs one statement. For the last statement of a run, a result cut at
// the limit cancels the query instead of reading the rest of the rows, which
// on a big table can take minutes. Cancelling may cost the connection (the
// MySQL driver drops it), so earlier statements read to the end to keep the
// session for the ones after them.
func (s *sqlDB) run(ctx context.Context, conn *sql.Conn, stmt string, limit int, last bool) Result {
	r := Result{Statement: stmt}
	start := time.Now()
	kw := firstKeyword(stmt)
	var err error
	switch {
	case s.readOnly && !slices.Contains(readKeywords, kw):
		err = errReadOnlyStmt
	case slices.Contains(rowKeywords, kw) || strings.HasPrefix(kw, "sp_"):
		qctx, cancel := context.WithCancel(ctx)
		var rows *sql.Rows
		rows, err = conn.QueryContext(qctx, stmt)
		if err == nil {
			stop := func() {}
			if last {
				stop = cancel
			}
			err = readSQLRows(rows, limit, &r, stop)
		}
		cut := last && r.Truncated
		cancel()
		if cut {
			err = nil // the error, if any, comes from our own cancel
		}
		if err == nil {
			r.Message = fmt.Sprintf("%d rows", len(r.Rows))
			var q sqlQueryer = conn
			if cut {
				q = s.db // the cancel may have closed conn
			}
			s.markEditable(ctx, q, stmt, &r)
		}
	default:
		var res sql.Result
		res, err = conn.ExecContext(ctx, stmt)
		if err == nil {
			n, _ := res.RowsAffected()
			r.Affected = n
			r.Message = fmt.Sprintf("%d rows affected", n)
		}
	}
	r.Millis = time.Since(start).Milliseconds()
	if err != nil {
		r.Error = err.Error()
	}
	return r
}

// markEditable sets r.Edit when stmt is a plain select from one table that
// has a primary key.
func (s *sqlDB) markEditable(ctx context.Context, conn sqlQueryer, stmt string, r *Result) {
	if s.readOnly || len(r.Columns) == 0 {
		return
	}
	table, ok := SingleTableSelect(stmt)
	if !ok {
		return
	}
	ref, err := s.eng.resolve(ctx, conn, table)
	if err != nil || !ref.isTable {
		return
	}
	r.Edit = editInfoFor(ref.object, "", r.Columns, ref.cols, ref.pk)
}

func (s *sqlDB) Explain(ctx context.Context, _ string, q string, analyze bool) ([]Result, error) {
	stmts := s.eng.split(q)
	if len(stmts) == 0 {
		return nil, errors.New("nothing to explain")
	}
	if s.readOnly && analyze && !slices.Contains(readKeywords, firstKeyword(stmts[0])) {
		return []Result{{Title: "Plan", Statement: stmts[0], Error: errReadOnlyStmt.Error()}}, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	start := time.Now()
	r := s.eng.explain(ctx, conn, stmts[0], analyze)
	r.Title, r.Statement = "Plan", stmts[0]
	r.Millis = time.Since(start).Milliseconds()
	return []Result{r}, nil
}

// Apply writes grid edits in one transaction.
func (s *sqlDB) Apply(ctx context.Context, _ string, c Changes) (Result, error) {
	if s.readOnly {
		return Result{}, ErrReadOnly
	}
	if len(c.Ops) == 0 {
		return Result{}, errors.New("no changes")
	}
	start := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	ref, err := s.eng.resolve(ctx, tx, c.Object)
	if err != nil {
		return Result{}, err
	}
	if !ref.isTable {
		return Result{}, fmt.Errorf("%s is not a table", c.Object)
	}
	var total int64
	for i, op := range c.Ops {
		stmt, args, err := buildEdit(s.eng.dialect(), ref.object, op)
		if err != nil {
			return Result{}, fmt.Errorf("change %d: %w", i+1, err)
		}
		res, err := tx.ExecContext(ctx, stmt, args...)
		if err != nil {
			return Result{}, fmt.Errorf("change %d (%s): %w", i+1, op.Op, err)
		}
		n, _ := res.RowsAffected()
		if err := checkAffected(i, op, n); err != nil {
			return Result{}, err
		}
		total += n
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	return Result{Message: editSummary(c), Affected: total, Millis: time.Since(start).Milliseconds()}, nil
}

// readSQLRows reads up to limit rows (0 means all) as text into r.
// readSQLRows reads up to limit rows into r. stop runs when the limit is
// hit, before the rows are closed, so the caller can abandon the rest.
func readSQLRows(rows *sql.Rows, limit int, r *Result, stop ...func()) error {
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	r.Columns = make([]Column, len(types))
	for i, t := range types {
		r.Columns[i] = Column{Name: t.Name(), Type: strings.ToLower(t.DatabaseTypeName())}
	}
	r.Rows = [][]any{}
	vals := make([]any, len(types))
	ptrs := make([]any, len(types))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if limit > 0 && len(r.Rows) >= limit {
			r.Truncated = true
			for _, f := range stop {
				f()
			}
			_ = rows.Close()
			return nil
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		row := make([]any, len(vals))
		for i, v := range vals {
			row[i] = sqlText(r.Columns[i].Type, v)
		}
		r.Rows = append(r.Rows, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return rows.Err()
}

var binaryTypes = []string{"binary", "varbinary", "blob", "tinyblob", "mediumblob", "longblob", "image", "rowversion", "geometry"}

// sqlText turns a scanned value into the text the grid shows, like
// Postgres' text output: numbers and times as strings, bytes as hex.
func sqlText(typ string, v any) any {
	switch v := v.(type) {
	case nil:
		return nil
	case []byte:
		if typ == "uniqueidentifier" && len(v) == 16 {
			return mssqlUUID(v)
		}
		if slices.Contains(binaryTypes, typ) || !utf8.Valid(v) {
			return "0x" + strings.ToUpper(hex.EncodeToString(v))
		}
		return string(v)
	case string:
		return v
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'g', -1, 32)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case time.Time:
		switch typ {
		case "date":
			return v.Format("2006-01-02")
		case "time":
			return v.Format("15:04:05.9999999")
		case "datetimeoffset":
			return v.Format("2006-01-02 15:04:05.9999999 -07:00")
		}
		return v.Format("2006-01-02 15:04:05.9999999")
	}
	return fmt.Sprint(v)
}

// mssqlUUID formats SQL Server's uniqueidentifier bytes, whose first three
// groups are little-endian.
func mssqlUUID(b []byte) string {
	u := []byte{b[3], b[2], b[1], b[0], b[5], b[4], b[7], b[6]}
	u = append(u, b[8:]...)
	h := strings.ToUpper(hex.EncodeToString(u))
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// queryResult runs a catalog query into a titled Result.
func queryResult(ctx context.Context, q sqlQueryer, title, query string, args ...any) (Result, error) {
	r := Result{Title: title}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return r, err
	}
	return r, readSQLRows(rows, 0, &r)
}

// queryStrings reads the first column of every row.
func queryStrings(ctx context.Context, q sqlQueryer, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// splitQualified splits a possibly quoted, dotted name such as
// `shop`.`users`, [dbo].[Orders] or main."my table" into its parts.
func splitQualified(s string) ([]string, error) {
	var parts []string
	r := []rune(strings.TrimSpace(s))
	for i := 0; i < len(r); {
		for i < len(r) && (r[i] == ' ' || r[i] == '\t') {
			i++
		}
		var part strings.Builder
		closeQ := map[rune]rune{'"': '"', '`': '`', '[': ']'}[at(r, i)]
		if closeQ != 0 {
			i++
			for {
				if i >= len(r) {
					return nil, fmt.Errorf("unterminated quote in %q", s)
				}
				if r[i] == closeQ {
					if at(r, i+1) == closeQ {
						part.WriteRune(closeQ)
						i += 2
						continue
					}
					i++
					break
				}
				part.WriteRune(r[i])
				i++
			}
		} else {
			for i < len(r) && r[i] != '.' && r[i] != ' ' {
				part.WriteRune(r[i])
				i++
			}
		}
		for i < len(r) && r[i] == ' ' {
			i++
		}
		if part.Len() == 0 {
			return nil, fmt.Errorf("bad name %q", s)
		}
		parts = append(parts, part.String())
		if i < len(r) {
			if r[i] != '.' {
				return nil, fmt.Errorf("bad name %q", s)
			}
			i++
			if i >= len(r) {
				return nil, fmt.Errorf("bad name %q", s)
			}
		}
	}
	if len(parts) == 0 {
		return nil, errors.New("name a table")
	}
	return parts, nil
}

// groupTree turns (group, object, kind, ref) rows, ordered by group, into tree nodes.
type treeRow struct{ group, name, kind, object string }

func groupTree(rows []treeRow, groupKind string) []Node {
	var out []Node
	for _, r := range rows {
		if len(out) == 0 || out[len(out)-1].Name != r.group {
			out = append(out, Node{Name: r.group, Kind: groupKind})
		}
		if r.name != "" {
			last := &out[len(out)-1]
			last.Children = append(last.Children, Node{Name: r.name, Kind: r.kind, Object: r.object})
		}
	}
	return out
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
