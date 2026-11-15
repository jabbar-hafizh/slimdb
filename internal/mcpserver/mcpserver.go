// Package mcpserver exposes the saved connections to AI agents over the
// Model Context Protocol (stdio transport).
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jabbar-hafizh/slimdb/internal/db"
	"github.com/jabbar-hafizh/slimdb/internal/store"
)

const (
	defaultLimit = 100
	maxLimit     = 1000
)

type server struct {
	store *store.Store
	dbs   *db.Manager
}

func Run(ctx context.Context, st *store.Store, dbs *db.Manager, version string) error {
	s := &server{store: st, dbs: dbs}
	m := mcp.NewServer(&mcp.Implementation{Name: "slimdb", Version: version}, nil)

	mcp.AddTool(m, &mcp.Tool{
		Name:        "list_connections",
		Description: "List the saved database connections (name, kind, read-only flag). Connection URLs are not returned.",
	}, s.listConnections)
	mcp.AddTool(m, &mcp.Tool{
		Name: "add_connection",
		Description: "Save a new connection. The URL starts with postgres://, mysql:// (also MariaDB), sqlserver://, sqlite: " +
			"(e.g. sqlite:///path/app.db), mongodb:// or mongodb+srv://, or redis:// (rediss:// for TLS).",
	}, s.addConnection)
	mcp.AddTool(m, &mcp.Tool{
		Name: "list_objects",
		Description: "List schemas or databases and their tables and views (SQL databases), databases and their collections (MongoDB), " +
			"or databases and their keys with each key's type (Redis, at most 1000 keys per database).",
	}, s.listObjects)
	mcp.AddTool(m, &mcp.Tool{
		Name: "describe",
		Description: "Describe a table, collection or key. SQL databases: columns, indexes and constraints of e.g. \"public.users\" " +
			"(Postgres), \"shop.users\" (MySQL), \"dbo.Orders\" (SQL Server) or \"users\" (SQLite). " +
			"MongoDB: fields inferred from a sample, and indexes. Redis: type, TTL and value of a key.",
	}, s.describe)
	mcp.AddTool(m, &mcp.Tool{
		Name: "query",
		Description: "Run one or more statements and return the results. SQL databases take SQL separated by semicolons " +
			"(SQL Server also accepts GO lines). Redis takes one command per line, e.g. HGETALL user:1. " +
			"MongoDB takes mongosh syntax, e.g. db.users.find({age: {$gt: 30}}).sort({name: 1}), " +
			"db.orders.aggregate([...]), db.users.updateOne({...}, {$set: {...}}), db.runCommand({...}), show collections. " +
			"Statements run in order and stop at the first error. Writes are allowed unless the connection is read-only.",
	}, s.query)
	mcp.AddTool(m, &mcp.Tool{
		Name: "explain",
		Description: "Show the query plan of the first statement. With analyze=true the statement really runs " +
			"(Postgres, MySQL and SQL Server roll it back; MongoDB uses executionStats). Not available for Redis.",
	}, s.explain)

	return m.Run(ctx, &mcp.StdioTransport{})
}

type noInput struct{}

type addConnectionInput struct {
	Name     string `json:"name" jsonschema:"a short unique name for the connection"`
	URL      string `json:"url" jsonschema:"connection URL, e.g. postgres://user:pass@localhost:5432/app"`
	ReadOnly bool   `json:"readOnly,omitempty" jsonschema:"refuse writes on this connection"`
}

type objectsInput struct {
	Connection string `json:"connection" jsonschema:"connection name or id"`
}

type describeInput struct {
	Connection string `json:"connection" jsonschema:"connection name or id"`
	Object     string `json:"object" jsonschema:"SQL: schema.table (or table), MongoDB: collection name, Redis: key"`
	Database   string `json:"database,omitempty" jsonschema:"MongoDB database, or Redis database number; defaults to the one in the connection URL"`
}

type queryInput struct {
	Connection string `json:"connection" jsonschema:"connection name or id"`
	Query      string `json:"query" jsonschema:"SQL for SQL databases, mongosh syntax for MongoDB, commands for Redis"`
	Database   string `json:"database,omitempty" jsonschema:"MongoDB database, or Redis database number; defaults to the one in the connection URL"`
	Limit      int    `json:"limit,omitempty" jsonschema:"maximum rows per statement, default 100, max 1000"`
}

type explainInput struct {
	Connection string `json:"connection" jsonschema:"connection name or id"`
	Query      string `json:"query" jsonschema:"the statement to explain"`
	Database   string `json:"database,omitempty" jsonschema:"MongoDB database, or Redis database number; defaults to the one in the connection URL"`
	Analyze    bool   `json:"analyze,omitempty" jsonschema:"run the statement to get real timings"`
}

func (s *server) listConnections(_ context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
	conns, err := s.store.Conns()
	if err != nil {
		return errorResult(err), nil, nil
	}
	type item struct {
		Name     string `json:"name"`
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		ReadOnly bool   `json:"readOnly"`
	}
	out := []item{}
	for _, c := range conns {
		out = append(out, item{c.Name, c.ID, c.Kind, c.ReadOnly})
	}
	return jsonResult(out), nil, nil
}

func (s *server) addConnection(ctx context.Context, _ *mcp.CallToolRequest, in addConnectionInput) (*mcp.CallToolResult, any, error) {
	c := store.Conn{Name: in.Name, URL: in.URL, ReadOnly: in.ReadOnly}
	kind, err := store.KindFromURL(strings.TrimSpace(c.URL))
	if err != nil {
		return errorResult(err), nil, nil
	}
	c.Kind = kind
	d, err := db.Open(ctx, c)
	if err != nil {
		return errorResult(fmt.Errorf("could not connect, nothing saved: %w", err)), nil, nil
	}
	d.Close()
	saved, err := s.store.Save(c)
	if err != nil {
		return errorResult(err), nil, nil
	}
	return textResult(fmt.Sprintf("saved %s connection %q (id %s)", saved.Kind, saved.Name, saved.ID)), nil, nil
}

func (s *server) listObjects(ctx context.Context, _ *mcp.CallToolRequest, in objectsInput) (*mcp.CallToolResult, any, error) {
	_, d, err := s.open(ctx, in.Connection)
	if err != nil {
		return errorResult(err), nil, nil
	}
	nodes, err := d.Tree(ctx)
	if err != nil {
		return errorResult(err), nil, nil
	}
	// One line per schema or database keeps this compact for large databases.
	var sb strings.Builder
	for _, n := range nodes {
		fmt.Fprintf(&sb, "%s %s:", n.Kind, n.Name)
		if len(n.Children) == 0 {
			sb.WriteString(" (empty)")
		}
		for i, c := range n.Children {
			sep := ","
			if i == 0 {
				sep = ""
			}
			label := c.Name
			if c.Kind != "table" && c.Kind != "collection" {
				label += " (" + c.Kind + ")"
			}
			fmt.Fprintf(&sb, "%s %s", sep, label)
		}
		sb.WriteByte('\n')
	}
	return textResult(sb.String()), nil, nil
}

func (s *server) describe(ctx context.Context, _ *mcp.CallToolRequest, in describeInput) (*mcp.CallToolResult, any, error) {
	_, d, err := s.open(ctx, in.Connection)
	if err != nil {
		return errorResult(err), nil, nil
	}
	results, err := d.Describe(ctx, in.Database, in.Object)
	if err != nil {
		return errorResult(err), nil, nil
	}
	return jsonResult(compact(results)), nil, nil
}

func (s *server) query(ctx context.Context, _ *mcp.CallToolRequest, in queryInput) (*mcp.CallToolResult, any, error) {
	c, d, err := s.open(ctx, in.Connection)
	if err != nil {
		return errorResult(err), nil, nil
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maxLimit)

	start := time.Now()
	results, err := d.Query(ctx, in.Database, in.Query, limit)
	entry := store.HistoryEntry{
		ConnID: c.ID, Database: in.Database, Query: in.Query, Source: "mcp",
		At: start, Millis: time.Since(start).Milliseconds(),
	}
	if err == nil {
		entry.Error = lastError(results)
	} else {
		entry.Error = err.Error()
	}
	s.store.AddHistory(entry)
	if err != nil {
		return errorResult(err), nil, nil
	}
	res := jsonResult(compact(results))
	res.IsError = lastError(results) != ""
	return res, nil, nil
}

func (s *server) explain(ctx context.Context, _ *mcp.CallToolRequest, in explainInput) (*mcp.CallToolResult, any, error) {
	_, d, err := s.open(ctx, in.Connection)
	if err != nil {
		return errorResult(err), nil, nil
	}
	results, err := d.Explain(ctx, in.Database, in.Query, in.Analyze)
	if err != nil {
		return errorResult(err), nil, nil
	}
	return jsonResult(compact(results)), nil, nil
}

func (s *server) open(ctx context.Context, name string) (store.Conn, db.Driver, error) {
	c, err := s.store.Conn(name)
	if err != nil {
		return c, nil, fmt.Errorf("%w: %q (call list_connections to see the names)", err, name)
	}
	d, err := s.dbs.Get(ctx, c)
	return c, d, err
}

// compactResult drops what an agent does not need: MongoDB results carry the
// documents themselves instead of the flattened grid.
type compactResult struct {
	Title     string            `json:"title,omitempty"`
	Statement string            `json:"statement,omitempty"`
	Columns   []string          `json:"columns,omitempty"`
	Rows      [][]any           `json:"rows,omitempty"`
	Docs      []json.RawMessage `json:"docs,omitempty"`
	Affected  int64             `json:"affected,omitempty"`
	Truncated bool              `json:"truncated,omitempty"`
	Message   string            `json:"message,omitempty"`
	Error     string            `json:"error,omitempty"`
	Millis    int64             `json:"ms"`
}

func compact(results []db.Result) []compactResult {
	out := make([]compactResult, 0, len(results))
	for _, r := range results {
		c := compactResult{
			Title: r.Title, Statement: r.Statement, Affected: r.Affected,
			Truncated: r.Truncated, Message: r.Message, Error: r.Error, Millis: r.Millis,
		}
		if len(r.Docs) > 0 {
			for _, d := range r.Docs {
				c.Docs = append(c.Docs, json.RawMessage(d))
			}
		} else {
			for _, col := range r.Columns {
				c.Columns = append(c.Columns, col.Name)
			}
			c.Rows = r.Rows
		}
		out = append(out, c)
	}
	return out
}

func lastError(results []db.Result) string {
	if len(results) == 0 {
		return ""
	}
	return results[len(results)-1].Error
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func jsonResult(v any) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return errorResult(err)
	}
	return textResult(string(b))
}

func errorResult(err error) *mcp.CallToolResult {
	r := textResult(err.Error())
	r.IsError = true
	return r
}
