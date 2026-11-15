package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// mysqlEngine covers MySQL and MariaDB.
type mysqlEngine struct{}

// mysqlConfig turns mysql://user:pass@host:3306/db?tls=true into the
// driver's config. Query parameters are passed through as driver options.
func mysqlConfig(raw string) (*mysql.Config, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	if u.Port() == "" {
		cfg.Addr = u.Hostname() + ":3306"
	}
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
	}
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	dsn := cfg.FormatDSN()
	if u.RawQuery != "" {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + u.RawQuery
	}
	cfg, err = mysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ParseTime = false
	cfg.MultiStatements = false
	// An UPDATE that sets the value a row already has must count as a hit,
	// or the grid's one-row check would fail.
	cfg.ClientFoundRows = true
	return cfg, nil
}

// openMySQL connects to MySQL or MariaDB. readOnly makes every session's
// transactions read-only on the server, on top of the statement check.
func openMySQL(ctx context.Context, raw string, readOnly bool) (*sqlDB, error) {
	cfg, err := mysqlConfig(raw)
	if err != nil {
		return nil, err
	}
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	// MySQL and MariaDB 11.1+ call it transaction_read_only, older MariaDB tx_read_only.
	vars := []string{""}
	if readOnly {
		vars = []string{"transaction_read_only", "tx_read_only"}
	}
	var lastErr error
	for _, v := range vars {
		c := cfg.Clone()
		if v != "" {
			c.Params[v] = "1"
		}
		conn, err := mysql.NewConnector(c)
		if err != nil {
			return nil, err
		}
		db := sql.OpenDB(conn)
		db.SetMaxOpenConns(4)
		if err := db.PingContext(ctx); err != nil {
			db.Close()
			lastErr = err
			var me *mysql.MySQLError
			if errors.As(err, &me) && me.Number == 1193 { // unknown system variable
				continue
			}
			return nil, err
		}
		return &sqlDB{db: db, eng: mysqlEngine{}, readOnly: readOnly}, nil
	}
	return nil, lastErr
}

func (mysqlEngine) dialect() sqlDialect { return dialectMySQL }

func (mysqlEngine) split(script string) []string { return splitSQL(script, splitOpts{mysql: true}) }

const mysqlSystemSchemas = `('mysql', 'information_schema', 'performance_schema', 'sys')`

func (mysqlEngine) tree(ctx context.Context, db *sql.DB) ([]Node, error) {
	rows, err := db.QueryContext(ctx, `
		select s.schema_name, coalesce(t.table_name, ''), coalesce(t.table_type, '')
		from information_schema.schemata s
		left join information_schema.tables t on t.table_schema = s.schema_name
		where s.schema_name not in `+mysqlSystemSchemas+`
		order by s.schema_name, t.table_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	q := dialectMySQL.quote
	var out []treeRow
	for rows.Next() {
		var schema, name, typ string
		if err := rows.Scan(&schema, &name, &typ); err != nil {
			return nil, err
		}
		kind := "table"
		if typ == "VIEW" {
			kind = "view"
		}
		out = append(out, treeRow{group: schema, name: name, kind: kind, object: q(schema) + "." + q(name)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return groupTree(out, "database"), nil
}

func (mysqlEngine) resolve(ctx context.Context, q sqlQueryer, ref string) (tableRef, error) {
	parts, err := splitQualified(ref)
	if err != nil {
		return tableRef{}, err
	}
	var schema, name string
	switch len(parts) {
	case 1:
		var cur sql.NullString
		if err := q.QueryRowContext(ctx, "select database()").Scan(&cur); err != nil {
			return tableRef{}, err
		}
		if !cur.Valid {
			return tableRef{}, fmt.Errorf("no database selected, name the table as database.%s", parts[0])
		}
		schema, name = cur.String, parts[0]
	case 2:
		schema, name = parts[0], parts[1]
	default:
		return tableRef{}, fmt.Errorf("bad table name %q", ref)
	}
	var typ string
	err = q.QueryRowContext(ctx, `select table_schema, table_name, table_type from information_schema.tables
		where table_schema = ? and table_name = ?`, schema, name).Scan(&schema, &name, &typ)
	if errors.Is(err, sql.ErrNoRows) {
		return tableRef{}, fmt.Errorf("table %s not found", ref)
	}
	if err != nil {
		return tableRef{}, err
	}
	t := tableRef{schema: schema, name: name, isTable: typ != "VIEW",
		object: dialectMySQL.quote(schema) + "." + dialectMySQL.quote(name)}
	if t.cols, err = queryStrings(ctx, q, `select column_name from information_schema.columns
		where table_schema = ? and table_name = ? order by ordinal_position`, schema, name); err != nil {
		return t, err
	}
	t.pk, err = queryStrings(ctx, q, `select column_name from information_schema.key_column_usage
		where table_schema = ? and table_name = ? and constraint_name = 'PRIMARY' order by ordinal_position`, schema, name)
	return t, err
}

func (mysqlEngine) describe(ctx context.Context, q sqlQueryer, t tableRef) ([]Result, error) {
	sections := []struct{ title, sql string }{
		{"Columns", `select column_name as name, column_type as type,
				case when is_nullable = 'YES' then 'true' else 'false' end as nullable,
				column_default as ` + "`default`" + `,
				case when column_key = 'PRI' then 'true' else 'false' end as primary_key,
				nullif(extra, '') as extra, nullif(column_comment, '') as comment
			from information_schema.columns where table_schema = ? and table_name = ?
			order by ordinal_position`},
		{"Indexes", `select index_name as name, case when non_unique = 0 then 'true' else 'false' end as ` + "`unique`" + `,
				group_concat(column_name order by seq_in_index separator ', ') as columns, index_type as type
			from information_schema.statistics where table_schema = ? and table_name = ?
			group by index_name, non_unique, index_type order by index_name`},
		{"Foreign keys", `select constraint_name as name,
				group_concat(column_name order by ordinal_position separator ', ') as columns,
				concat(referenced_table_schema, '.', referenced_table_name, ' (',
					group_concat(referenced_column_name order by ordinal_position separator ', '), ')') as ` + "`references`" + `
			from information_schema.key_column_usage
			where table_schema = ? and table_name = ? and referenced_table_name is not null
			group by constraint_name, referenced_table_schema, referenced_table_name order by constraint_name`},
	}
	var out []Result
	for _, s := range sections {
		r, err := queryResult(ctx, q, s.title, s.sql, t.schema, t.name)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// explain uses EXPLAIN, or EXPLAIN ANALYZE (MySQL 8.0.18+) / ANALYZE
// (MariaDB) inside a transaction that is rolled back, since both really run
// the statement.
func (mysqlEngine) explain(ctx context.Context, conn *sql.Conn, stmt string, analyze bool) Result {
	var r Result
	run := func(q sqlQueryer, sqlText string) error {
		rows, err := q.QueryContext(ctx, sqlText)
		if err != nil {
			return err
		}
		return readSQLRows(rows, 0, &r)
	}
	if !analyze {
		if err := run(conn, "explain "+stmt); err != nil {
			r.Error = err.Error()
		}
		return r
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer tx.Rollback() //nolint:errcheck // rollback is the point
	err = run(tx, "explain analyze "+stmt)
	var me *mysql.MySQLError
	if err != nil && errors.As(err, &me) && me.Number == 1064 { // syntax error: MariaDB
		r = Result{}
		err = run(tx, "analyze "+stmt)
	}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}
