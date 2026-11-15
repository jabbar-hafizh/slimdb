package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	_ "github.com/microsoft/go-mssqldb" // registers the "sqlserver" driver
)

type mssqlEngine struct{}

// openSQLServer takes the driver's own URL form,
// sqlserver://user:pass@host:1433?database=app.
//
// readOnly is enforced by the statement check only: SQL Server has no
// session switch for it. Use a login with read access for a hard guarantee.
func openSQLServer(ctx context.Context, raw string, readOnly bool) (*sqlDB, error) {
	db, err := sql.Open("sqlserver", raw)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return &sqlDB{db: db, eng: mssqlEngine{}, readOnly: readOnly}, nil
}

func (mssqlEngine) dialect() sqlDialect { return dialectMSSQL }

var goLineRE = regexp.MustCompile(`(?im)^[ \t]*go[ \t]*$`)

// splitTSQL splits on GO lines (as SSMS does) and on semicolons.
func splitTSQL(script string) []string {
	var out []string
	for _, batch := range goLineRE.Split(script, -1) {
		out = append(out, splitSQL(batch, splitOpts{brackets: true})...)
	}
	return out
}

func (mssqlEngine) split(script string) []string { return splitTSQL(script) }

func (mssqlEngine) tree(ctx context.Context, db *sql.DB) ([]Node, error) {
	rows, err := db.QueryContext(ctx, `
		select s.name, coalesce(o.name, ''), coalesce(rtrim(o.type), ''),
			coalesce(quotename(s.name) + '.' + quotename(o.name), '')
		from sys.schemas s
		left join sys.objects o on o.schema_id = s.schema_id and o.type in ('U', 'V') and o.is_ms_shipped = 0
		where s.name not in ('sys', 'INFORMATION_SCHEMA', 'guest')
			and s.name not like 'db[_]%'
			and (o.object_id is not null or s.name = 'dbo')
		order by s.name, o.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []treeRow
	for rows.Next() {
		var schema, name, typ, object string
		if err := rows.Scan(&schema, &name, &typ, &object); err != nil {
			return nil, err
		}
		kind := "table"
		if typ == "V" {
			kind = "view"
		}
		out = append(out, treeRow{group: schema, name: name, kind: kind, object: object})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return groupTree(out, "schema"), nil
}

// resolve lets SQL Server itself parse the name with OBJECT_ID, so every
// form it accepts (dbo.t, [dbo].[t], t) works.
func (mssqlEngine) resolve(ctx context.Context, q sqlQueryer, ref string) (tableRef, error) {
	if _, err := splitQualified(ref); err != nil {
		return tableRef{}, err
	}
	var t tableRef
	var typ string
	err := q.QueryRowContext(ctx, `select schema_name(o.schema_id), o.name, rtrim(o.type),
			quotename(schema_name(o.schema_id)) + '.' + quotename(o.name)
		from sys.objects o where o.object_id = object_id(@p1) and o.type in ('U', 'V')`, ref).
		Scan(&t.schema, &t.name, &typ, &t.object)
	if errors.Is(err, sql.ErrNoRows) {
		return tableRef{}, fmt.Errorf("table %s not found", ref)
	}
	if err != nil {
		return tableRef{}, err
	}
	t.isTable = typ == "U"
	if t.cols, err = queryStrings(ctx, q, `select name from sys.columns where object_id = object_id(@p1) order by column_id`, t.object); err != nil {
		return t, err
	}
	t.pk, err = queryStrings(ctx, q, `select c.name from sys.indexes i
		join sys.index_columns ic on ic.object_id = i.object_id and ic.index_id = i.index_id
		join sys.columns c on c.object_id = ic.object_id and c.column_id = ic.column_id
		where i.object_id = object_id(@p1) and i.is_primary_key = 1 order by ic.key_ordinal`, t.object)
	return t, err
}

func (mssqlEngine) describe(ctx context.Context, q sqlQueryer, t tableRef) ([]Result, error) {
	sections := []struct{ title, sql string }{
		{"Columns", `select c.name,
				ty.name + case
					when ty.name in ('varchar', 'char', 'varbinary', 'binary')
						then '(' + case when c.max_length = -1 then 'max' else cast(c.max_length as varchar) end + ')'
					when ty.name in ('nvarchar', 'nchar')
						then '(' + case when c.max_length = -1 then 'max' else cast(c.max_length / 2 as varchar) end + ')'
					when ty.name in ('decimal', 'numeric')
						then '(' + cast(c.precision as varchar) + ',' + cast(c.scale as varchar) + ')'
					else '' end as type,
				case when c.is_nullable = 1 then 'true' else 'false' end as nullable,
				object_definition(c.default_object_id) as [default],
				case when exists (select 1 from sys.indexes i
					join sys.index_columns ic on ic.object_id = i.object_id and ic.index_id = i.index_id
					where i.object_id = c.object_id and i.is_primary_key = 1 and ic.column_id = c.column_id)
					then 'true' else 'false' end as primary_key,
				case when c.is_identity = 1 then 'identity' end as extra
			from sys.columns c join sys.types ty on ty.user_type_id = c.user_type_id
			where c.object_id = object_id(@p1) order by c.column_id`},
		{"Indexes", `select i.name, i.type_desc as type, case when i.is_unique = 1 then 'true' else 'false' end as [unique],
				(select string_agg(c.name, ', ') within group (order by ic.key_ordinal)
					from sys.index_columns ic join sys.columns c on c.object_id = ic.object_id and c.column_id = ic.column_id
					where ic.object_id = i.object_id and ic.index_id = i.index_id and ic.is_included_column = 0) as columns
			from sys.indexes i where i.object_id = object_id(@p1) and i.type > 0 order by i.name`},
		{"Foreign keys", `select fk.name,
				(select string_agg(c.name, ', ') from sys.foreign_key_columns fc
					join sys.columns c on c.object_id = fc.parent_object_id and c.column_id = fc.parent_column_id
					where fc.constraint_object_id = fk.object_id) as columns,
				quotename(schema_name(r.schema_id)) + '.' + quotename(r.name) as [references]
			from sys.foreign_keys fk join sys.objects r on r.object_id = fk.referenced_object_id
			where fk.parent_object_id = object_id(@p1) order by fk.name`},
	}
	var out []Result
	for _, s := range sections {
		r, err := queryResult(ctx, q, s.title, s.sql, t.object)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// explain turns on SHOWPLAN_TEXT, which returns the estimated plan without
// running the statement. With analyze the statement runs under STATISTICS
// PROFILE in a transaction that is rolled back.
func (mssqlEngine) explain(ctx context.Context, conn *sql.Conn, stmt string, analyze bool) Result {
	var r Result
	fail := func(err error) Result {
		r.Error = err.Error()
		return r
	}
	if !analyze {
		if _, err := conn.ExecContext(ctx, "set showplan_text on"); err != nil {
			return fail(err)
		}
		defer conn.ExecContext(context.WithoutCancel(ctx), "set showplan_text off") //nolint:errcheck
		rows, err := conn.QueryContext(ctx, stmt)
		if err != nil {
			return fail(err)
		}
		// The first result set echoes the statement, the second is the plan.
		if err := readLastResultSet(rows, &r); err != nil {
			return fail(err)
		}
		return r
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback is the point
	if _, err := tx.ExecContext(ctx, "set statistics profile on"); err != nil {
		return fail(err)
	}
	rows, err := tx.QueryContext(ctx, stmt)
	if err != nil {
		return fail(err)
	}
	if err := readLastResultSet(rows, &r); err != nil {
		return fail(err)
	}
	_, _ = tx.ExecContext(ctx, "set statistics profile off")
	return r
}

// readLastResultSet keeps the last result set that has columns.
func readLastResultSet(rows *sql.Rows, r *Result) error {
	defer rows.Close()
	for {
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		if len(cols) > 0 {
			var cur Result
			if err := readSet(rows, &cur); err != nil {
				return err
			}
			*r = cur
		}
		if !rows.NextResultSet() {
			return rows.Err()
		}
	}
}

// readSet reads the current result set without closing rows.
func readSet(rows *sql.Rows, r *Result) error {
	types, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	r.Columns = make([]Column, len(types))
	for i, t := range types {
		r.Columns[i] = Column{Name: t.Name()}
	}
	r.Rows = [][]any{}
	vals := make([]any, len(types))
	ptrs := make([]any, len(types))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		row := make([]any, len(vals))
		for i, v := range vals {
			row[i] = sqlText("", v)
		}
		r.Rows = append(r.Rows, row)
	}
	return rows.Err()
}
