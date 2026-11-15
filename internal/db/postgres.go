package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const pgCursor = "slimdb_cursor"

type postgres struct {
	pool *pgxpool.Pool
}

// openPostgres uses the simple query protocol everywhere, so every value
// comes back as Postgres' own text form, exactly what psql would print.
//
// readOnly sets default_transaction_read_only. That is a guard against
// mistakes, not a security boundary: use a read-only role for that.
func openPostgres(ctx context.Context, url string, readOnly bool) (*postgres, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 4
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.ConnConfig.RuntimeParams["application_name"] = "slimdb"
	if readOnly {
		cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &postgres{pool: pool}, nil
}

func (p *postgres) Close() { p.pool.Close() }

// querier is what *pgx.Conn and pgx.Tx have in common.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func (p *postgres) Tree(ctx context.Context) ([]Node, error) {
	const q = `
		select n.nspname, coalesce(c.relname, ''), coalesce(c.relkind::text, ''),
			coalesce(quote_ident(n.nspname) || '.' || quote_ident(c.relname), '')
		from pg_namespace n
		left join pg_class c on c.relnamespace = n.oid
			and c.relkind in ('r', 'p', 'v', 'm', 'f') and not c.relispartition
		where n.nspname not in ('pg_catalog', 'information_schema')
			and n.nspname not like 'pg\_toast%' and n.nspname not like 'pg\_temp%'
		order by 1, 2`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	kinds := map[string]string{"r": "table", "p": "table", "v": "view", "m": "matview", "f": "foreign"}
	var schemas []Node
	for rows.Next() {
		var schema, rel, kind, object string
		if err := rows.Scan(&schema, &rel, &kind, &object); err != nil {
			return nil, err
		}
		if len(schemas) == 0 || schemas[len(schemas)-1].Name != schema {
			schemas = append(schemas, Node{Name: schema, Kind: "schema"})
		}
		if rel != "" {
			last := &schemas[len(schemas)-1]
			last.Children = append(last.Children, Node{
				Name:   rel,
				Kind:   kinds[kind],
				Object: object, // quoted only where needed, e.g. public."Order"

			})
		}
	}
	return schemas, rows.Err()
}

func (p *postgres) Describe(ctx context.Context, _ string, object string) ([]Result, error) {
	var exists bool
	if err := p.pool.QueryRow(ctx, `select to_regclass($1) is not null`, object).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("relation %s not found", object)
	}
	sections := []struct{ title, sql string }{
		{"Columns", `
			select a.attname as name,
				format_type(a.atttypid, a.atttypmod) as type,
				not a.attnotnull as nullable,
				pg_get_expr(d.adbin, d.adrelid) as default,
				exists (select 1 from pg_index i where i.indrelid = a.attrelid
					and i.indisprimary and a.attnum = any(i.indkey)) as primary_key,
				col_description(a.attrelid, a.attnum) as comment
			from pg_attribute a
			left join pg_attrdef d on d.adrelid = a.attrelid and d.adnum = a.attnum
			where a.attrelid = to_regclass($1) and a.attnum > 0 and not a.attisdropped
			order by a.attnum`},
		{"Indexes", `
			select c.relname as name, pg_get_indexdef(i.indexrelid) as definition
			from pg_index i join pg_class c on c.oid = i.indexrelid
			where i.indrelid = to_regclass($1)
			order by 1`},
		{"Constraints", `
			select conname as name, pg_get_constraintdef(oid) as definition
			from pg_constraint
			where conrelid = to_regclass($1)
			order by 1`},
	}
	out := make([]Result, 0, len(sections))
	for _, s := range sections {
		r := Result{Title: s.title}
		if err := readRows(ctx, p.pool, s.sql, 0, &r, object); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Query runs the statements on one pooled connection, so session state such
// as SET, temp tables or an explicit BEGIN carries across them.
func (p *postgres) Query(ctx context.Context, _ string, q string, limit int) ([]Result, error) {
	stmts := SplitSQL(q)
	if len(stmts) == 0 {
		return nil, errors.New("nothing to run")
	}
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	var out []Result
	for _, stmt := range stmts {
		r := runStatement(ctx, conn.Conn(), stmt, limit)
		out = append(out, r)
		if r.Error != "" {
			break
		}
	}
	return out, nil
}

func runStatement(ctx context.Context, c *pgx.Conn, stmt string, limit int) Result {
	r := Result{Statement: stmt}
	start := time.Now()
	var err error
	switch firstKeyword(stmt) {
	case "select", "with", "values", "table":
		err = runWithCursor(ctx, c, stmt, limit, &r)
	default:
		err = readRows(ctx, c, stmt, limit, &r)
	}
	r.Millis = time.Since(start).Milliseconds()
	if err != nil {
		r.Error = err.Error()
	} else if c.PgConn().TxStatus() == 'I' {
		// Outside a user transaction only: a failed lookup must not abort it.
		r.Edit = pgEditInfo(ctx, c, r.Columns, r.src)
	}
	return r
}

// pgEditInfo makes a result editable when every column comes straight from
// one table and the table's primary key is among them. Postgres reports the
// source table and column of each result column, so aliases are fine.
func pgEditInfo(ctx context.Context, c *pgx.Conn, cols []Column, src []columnSource) *EditInfo {
	if len(src) == 0 || len(src) != len(cols) {
		return nil
	}
	table := src[0].table
	for _, s := range src {
		if s.table != table && s.table != 0 {
			return nil // more than one table
		}
	}
	if table == 0 {
		return nil
	}
	var object string
	var kind string
	var names []string
	var nums []int16
	var pk []int16
	err := c.QueryRow(ctx, `
		select format('%I.%I', n.nspname, t.relname), t.relkind::text,
			array(select attname::text from pg_attribute where attrelid = t.oid and attnum > 0 and not attisdropped order by attnum),
			array(select attnum from pg_attribute where attrelid = t.oid and attnum > 0 and not attisdropped order by attnum),
			coalesce((select i.indkey::int2[] from pg_index i where i.indrelid = t.oid and i.indisprimary), '{}')
		from pg_class t join pg_namespace n on n.oid = t.relnamespace
		where t.oid = $1`, table).Scan(&object, &kind, &names, &nums, &pk)
	if err != nil || (kind != "r" && kind != "p") || len(pk) == 0 {
		return nil
	}
	byNum := map[int16]string{}
	for i, n := range nums {
		byNum[n] = names[i]
	}
	info := &EditInfo{Object: object, Columns: make([]string, len(cols))}
	seen := map[string]bool{}
	for i, s := range src {
		name := ""
		if s.table == table {
			name = byNum[int16(s.attr)]
		}
		if name != "" && seen[name] {
			name = ""
		}
		seen[name] = true
		info.Columns[i] = name
	}
	for _, n := range pk {
		name := byNum[n]
		if !seen[name] || name == "" {
			return nil
		}
		info.Key = append(info.Key, name)
	}
	return info
}

// Apply writes grid edits in one transaction. Values travel as text and
// the simple protocol sends them as untyped literals, so Postgres casts them
// to each column's type the same way it would cast '42' in SQL.
func (p *postgres) Apply(ctx context.Context, _ string, c Changes) (Result, error) {
	start := time.Now()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	var object, kind string
	err = tx.QueryRow(ctx, `select format('%I.%I', n.nspname, t.relname), t.relkind::text
		from pg_class t join pg_namespace n on n.oid = t.relnamespace where t.oid = to_regclass($1)`, c.Object).Scan(&object, &kind)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && kind != "r" && kind != "p") {
		return Result{}, fmt.Errorf("table %s not found", c.Object)
	}
	if err != nil {
		return Result{}, err
	}
	var total int64
	for i, op := range c.Ops {
		sql, args, err := buildEdit(dialectPostgres, object, op)
		if err != nil {
			return Result{}, fmt.Errorf("change %d: %w", i+1, err)
		}
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			return Result{}, fmt.Errorf("change %d (%s): %w", i+1, op.Op, err)
		}
		if err := checkAffected(i, op, tag.RowsAffected()); err != nil {
			return Result{}, err
		}
		total += tag.RowsAffected()
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	return Result{Message: editSummary(c), Affected: total, Millis: time.Since(start).Milliseconds()}, nil
}

// runWithCursor fetches only limit+1 rows of a query, so a SELECT on a huge
// table does not stream the whole table to us. Statements a cursor cannot
// hold (SELECT INTO, data-modifying WITH) fall back to running directly.
func runWithCursor(ctx context.Context, c *pgx.Conn, stmt string, limit int, r *Result) error {
	inUserTx := c.PgConn().TxStatus() != 'I'
	if !inUserTx {
		if _, err := c.Exec(ctx, "begin"); err != nil {
			return err
		}
	}
	finish := func(err error) error {
		if inUserTx {
			if err == nil {
				_, err = c.Exec(ctx, "close "+pgCursor)
			}
			return err
		}
		if err != nil {
			_, _ = c.Exec(ctx, "rollback")
			return err
		}
		_, err = c.Exec(ctx, "commit")
		return err
	}

	if _, err := c.Exec(ctx, "declare "+pgCursor+" no scroll cursor for "+stmt); err != nil {
		if inUserTx {
			return err
		}
		_, _ = c.Exec(ctx, "rollback")
		return readRows(ctx, c, stmt, limit, r)
	}
	fetch := "fetch all from " + pgCursor
	if limit > 0 {
		fetch = "fetch forward " + strconv.Itoa(limit+1) + " from " + pgCursor
	}
	err := readRows(ctx, c, fetch, limit, r)
	if err == nil {
		r.Message = fmt.Sprintf("%d rows", len(r.Rows))
		r.Affected = 0
	}
	return finish(err)
}

// readRows reads up to limit rows (0 means all) as text into r.
func readRows(ctx context.Context, q querier, sql string, limit int, r *Result, args ...any) error {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	typeMap := rows.Conn().TypeMap()
	r.Columns = []Column{}
	for _, f := range rows.FieldDescriptions() {
		typ := strconv.FormatUint(uint64(f.DataTypeOID), 10)
		if t, ok := typeMap.TypeForOID(f.DataTypeOID); ok {
			typ = t.Name
		}
		r.Columns = append(r.Columns, Column{Name: f.Name, Type: typ})
		r.src = append(r.src, columnSource{table: f.TableOID, attr: f.TableAttributeNumber})
	}
	r.Rows = [][]any{}
	for rows.Next() {
		if limit > 0 && len(r.Rows) >= limit {
			r.Truncated = true
			break
		}
		raw := rows.RawValues()
		row := make([]any, len(raw))
		for i, b := range raw {
			if b != nil {
				row[i] = string(b)
			}
		}
		r.Rows = append(r.Rows, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	tag := rows.CommandTag()
	if !tag.Select() {
		r.Affected = tag.RowsAffected()
	}
	r.Message = tag.String()
	return nil
}

// Explain explains the first statement. With analyze the statement really
// runs, so it runs inside a transaction that is rolled back.
func (p *postgres) Explain(ctx context.Context, _ string, q string, analyze bool) ([]Result, error) {
	stmts := SplitSQL(q)
	if len(stmts) == 0 {
		return nil, errors.New("nothing to explain")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback is the point

	opts := "format text"
	if analyze {
		opts = "analyze, buffers, format text"
	}
	r := Result{Title: "Plan", Statement: stmts[0]}
	start := time.Now()
	err = readRows(ctx, tx, "explain ("+opts+") "+stmts[0], 0, &r)
	r.Millis = time.Since(start).Milliseconds()
	if err != nil {
		r.Error = err.Error()
	}
	return []Result{r}, nil
}
