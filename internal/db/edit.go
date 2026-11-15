package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Editing rows in the results grid.
//
// A driver marks a Result as editable by setting Result.Edit, when the rows
// come from one table (or collection) and every row can be found again by
// its key. The UI then sends Changes back, which the driver applies in one
// transaction through the optional Editor interface.

// EditInfo tells the UI how to write a result back.
type EditInfo struct {
	// Object is what Apply accepts, usually the same string Describe takes.
	Object   string `json:"object"`
	Database string `json:"database,omitempty"`
	// Key lists the table columns that identify a row, e.g. the primary key.
	// Each one appears in Columns.
	Key []string `json:"key"`
	// Columns maps each result column to its column in the table, or "" when
	// the result column cannot be written (an expression, for example).
	Columns []string `json:"columns"`
}

// Changes is one batch of grid edits for one object.
type Changes struct {
	Object string   `json:"object"`
	Ops    []EditOp `json:"ops"`
}

// EditOp is one row change. Key holds the row's key values, Values the new
// column values. SQL drivers get values as text (or nil for NULL) and let the
// database cast them; MongoDB gets relaxed Extended JSON.
type EditOp struct {
	Op     string         `json:"op"` // update, delete, insert
	Key    map[string]any `json:"key,omitempty"`
	Values map[string]any `json:"values,omitempty"`
}

// Editor is implemented by drivers whose results can be edited in the grid.
type Editor interface {
	Apply(ctx context.Context, database string, c Changes) (Result, error)
}

// ErrReadOnly is returned by Apply on a read-only connection.
var ErrReadOnly = errors.New("this connection is read-only")

// columnSource is where a result column comes from, as Postgres reports it.
type columnSource struct {
	table uint32
	attr  uint16
}

// sqlDialect is what differs between SQL databases when writing edits.
type sqlDialect struct {
	quote func(ident string) string
	param func(n int) string // n starts at 1
}

func quoteWith(open, close string) func(string) string {
	return func(s string) string {
		return open + strings.ReplaceAll(s, close, close+close) + close
	}
}

var (
	dialectPostgres = sqlDialect{quote: quoteWith(`"`, `"`), param: func(n int) string { return fmt.Sprintf("$%d", n) }}
	dialectMySQL    = sqlDialect{quote: quoteWith("`", "`"), param: func(int) string { return "?" }}
	dialectSQLite   = sqlDialect{quote: quoteWith(`"`, `"`), param: func(int) string { return "?" }}
	dialectMSSQL    = sqlDialect{quote: quoteWith("[", "]"), param: func(n int) string { return fmt.Sprintf("@p%d", n) }}
)

// buildEdit turns one op into a statement. object must already be a valid,
// quoted table reference (callers check it exists); column names are quoted
// here and values always travel as parameters.
func buildEdit(d sqlDialect, object string, op EditOp) (string, []any, error) {
	var args []any
	next := func(v any) string {
		args = append(args, v)
		return d.param(len(args))
	}
	where := func() (string, error) {
		if len(op.Key) == 0 {
			return "", errors.New("a row to change needs its key")
		}
		var parts []string
		for _, k := range sortedKeys(op.Key) {
			if v := op.Key[k]; v == nil {
				parts = append(parts, d.quote(k)+" is null")
			} else {
				parts = append(parts, d.quote(k)+" = "+next(v))
			}
		}
		return " where " + strings.Join(parts, " and "), nil
	}
	switch op.Op {
	case "update":
		if len(op.Values) == 0 {
			return "", nil, errors.New("an update needs at least one value")
		}
		var sets []string
		for _, k := range sortedKeys(op.Values) {
			sets = append(sets, d.quote(k)+" = "+next(op.Values[k]))
		}
		w, err := where()
		if err != nil {
			return "", nil, err
		}
		return "update " + object + " set " + strings.Join(sets, ", ") + w, args, nil
	case "delete":
		w, err := where()
		if err != nil {
			return "", nil, err
		}
		return "delete from " + object + w, args, nil
	case "insert":
		if len(op.Values) == 0 {
			return "", nil, errors.New("fill in at least one column of the new row")
		}
		var cols, vals []string
		for _, k := range sortedKeys(op.Values) {
			cols = append(cols, d.quote(k))
			vals = append(vals, next(op.Values[k]))
		}
		return "insert into " + object + " (" + strings.Join(cols, ", ") + ") values (" + strings.Join(vals, ", ") + ")", args, nil
	}
	return "", nil, fmt.Errorf("unknown edit %q", op.Op)
}

// checkAffected makes sure an update or delete hit exactly one row, so a
// key that is not unique after all never changes more than the user saw.
func checkAffected(i int, op EditOp, n int64) error {
	if op.Op == "insert" || n == 1 {
		return nil
	}
	return fmt.Errorf("change %d (%s) matched %d rows instead of 1, nothing was saved", i+1, op.Op, n)
}

func editSummary(c Changes) string {
	count := map[string]int{}
	for _, op := range c.Ops {
		count[op.Op]++
	}
	var parts []string
	for _, k := range []string{"update", "insert", "delete"} {
		if n := count[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", map[string]string{"update": "updated", "insert": "inserted", "delete": "deleted"}[k], n))
		}
	}
	return strings.Join(parts, ", ")
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// simpleSelectRE matches "select <list> from <table> [alias] [where|order|limit ...]",
// the shape a grid can write back to. Joins, grouping and set operations are
// ruled out separately.
var simpleSelectRE = regexp.MustCompile(`(?is)^\s*select\s+(?:top\s*\(?\s*\d+\s*\)?\s+)?(.+?)\s+from\s+((?:[\w$]+|"[^"]+"|` + "`[^`]+`" + `|\[[^\]]+\])(?:\s*\.\s*(?:[\w$]+|"[^"]+"|` + "`[^`]+`" + `|\[[^\]]+\]))*)(?:\s+(?:as\s+)?([\w$]+))?\s*(?:(?:where|order|limit|offset|fetch)\b.*)?;?\s*$`)

var selectItemRE = regexp.MustCompile(`^(?:[\w$]+\s*\.\s*)?(?:\*|[\w$]+|"[^"]+"|` + "`[^`]+`" + `|\[[^\]]+\])$`)

var notSimpleRE = regexp.MustCompile(`(?i)\b(join|union|intersect|except|group\s+by|having|distinct|window|over)\b`)

// SingleTableSelect reports the table a plain single-table SELECT reads, as
// written in the statement. Drivers without per-column table information use
// it to decide whether a result is editable.
func SingleTableSelect(stmt string) (table string, ok bool) {
	m := simpleSelectRE.FindStringSubmatch(stmt)
	if m == nil || notSimpleRE.MatchString(stmt) {
		return "", false
	}
	if alias := strings.ToLower(m[3]); alias != "" && slices.Contains([]string{"where", "order", "limit", "offset", "fetch"}, alias) {
		return "", false // the regex took a keyword for an alias; be safe
	}
	// The select list may only name columns: *, t.*, col, t.col.
	for _, item := range strings.Split(m[1], ",") {
		item = strings.TrimSpace(item)
		if !selectItemRE.MatchString(item) {
			return "", false
		}
	}
	return strings.TrimSpace(m[2]), true
}

// editInfoFor builds EditInfo for a result read from one table, given the
// table's columns and primary key. It returns nil when a key column is
// missing from the result, since such rows cannot be found again.
func editInfoFor(object, database string, result []Column, tableCols, pk []string) *EditInfo {
	if len(pk) == 0 {
		return nil
	}
	find := func(name string) string {
		for _, c := range tableCols {
			if strings.EqualFold(c, name) {
				return c
			}
		}
		return ""
	}
	info := &EditInfo{Object: object, Database: database, Columns: make([]string, len(result))}
	seen := map[string]bool{}
	for i, c := range result {
		col := find(c.Name)
		if col != "" && seen[col] {
			col = "" // the same column twice: only the first copy is editable
		}
		seen[col] = true
		info.Columns[i] = col
	}
	for _, k := range pk {
		if !slices.Contains(info.Columns, k) {
			return nil
		}
	}
	info.Key = pk
	return info
}
