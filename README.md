# slimdb

A slim, local database client for **PostgreSQL** and **MongoDB**, with a built-in
**MCP server** so AI agents (Claude Code, Claude Desktop, Cursor, and others) can
query your local databases too.

One binary. No Electron, no JVM, no account. It opens in your browser and only
listens on localhost.

![slimdb with a Postgres result](docs/screenshot-postgres.png)

## Features

- **Connections** for PostgreSQL and MongoDB, saved locally, with an optional read-only flag
- **Object tree**: schemas, tables and views (Postgres) or databases and collections (MongoDB), with a filter
- **Query console** per connection, kept between sessions
  - Postgres: plain SQL, several statements per run, explicit transactions work
  - MongoDB: mongosh syntax, e.g. `db.users.find({ age: { $gt: 30 } }).sort({ name: 1 })`
- **Results grid**: typed column headers, sort by column, NULLs marked, full value on double-click, JSON view for documents, copy as CSV or JSON
- **Describe**: columns, indexes and constraints (Postgres), inferred fields and indexes (MongoDB)
- **Explain**, optionally with `analyze` (Postgres rolls the statement back)
- **History** of everything run, from the UI and from AI agents
- **MCP server**: list connections, browse objects, describe, query, explain

## Install

Download a binary for macOS, Linux or Windows from
[Releases](https://github.com/jabbar-hafizh/slimdb/releases), or build it with Go 1.25+:

```sh
go install github.com/jabbar-hafizh/slimdb/cmd/slimdb@latest
```

### Docker

Runs in the background, starts with Docker, and keeps connections in a volume:

```sh
docker run -d --name slimdb --restart unless-stopped \
  -p 127.0.0.1:7788:7788 -v slimdb-data:/data \
  --add-host host.docker.internal:host-gateway \
  ghcr.io/jabbar-hafizh/slimdb:latest
```

or, from a clone of this repo, `docker compose up -d`. Then open http://localhost:7788.

- `localhost` in a connection URL means your machine, not the container: the image
  rewrites it to `host.docker.internal` (set `SLIMDB_LOCALHOST_ALIAS=` to turn that off).
  For databases in other containers, use their container or service name.
- Keep the port published on `127.0.0.1` as above. Anyone who can reach the port
  can run queries.
- Manage connections from the terminal with `docker exec slimdb slimdb conn add ...`.
- MCP through the running container: `claude mcp add slimdb -- docker exec -i slimdb slimdb mcp`

## Quick start

```sh
slimdb                       # opens http://127.0.0.1:7788 in your browser
```

Click **+** to add a connection, for example:

```
postgres://postgres:secret@localhost:5432/app
mongodb://localhost:27017/shop
```

Or add connections from the terminal:

```sh
slimdb conn add local-pg postgres://postgres:secret@localhost:5432/app
slimdb conn add -read-only prod-replica postgres://readonly@replica:5432/app
slimdb conn list
```

### Running queries

| Shortcut | Action |
| --- | --- |
| Ctrl/Cmd + Enter | Run the selection, or the block around the cursor (blocks are separated by a blank line) |
| Ctrl/Cmd + Shift + Enter | Run the whole editor |
| Click a table | Describe it |
| Double-click a table | Query its first 100 rows |
| Double-click a cell | Show the full value |

Results are capped by the **Limit** selector (default 500). Postgres SELECTs run
through a cursor, so a query on a huge table only fetches what is shown.

### MongoDB syntax

slimdb understands the common mongosh forms:

```js
show dbs
show collections
use shop

db.orders.find({ status: "paid" }, { items: 0 }).sort({ at: -1 }).limit(20)
db.orders.aggregate([{ $group: { _id: "$status", n: { $sum: 1 } } }])
db.orders.countDocuments({ at: { $gte: ISODate("2026-01-01") } })
db.orders.distinct("status")
db.orders.insertOne({ item: "coffee", qty: 2 })
db.orders.updateMany({ status: "new" }, { $set: { status: "paid" } })
db.orders.deleteOne({ _id: ObjectId("65a1b2c3d4e5f60718293a4b") })
db.orders.createIndex({ at: -1 }, { name: "at_desc" })
db.getCollection("order items").find()
db.runCommand({ dbStats: 1 })
```

Supported helpers: `ObjectId`, `ISODate`/`new Date`, `NumberLong`, `NumberInt`,
`NumberDecimal`, `UUID`, and regular expressions like `/^ab/i`. It is a parser for
these forms, not a JavaScript engine, so variables and loops are not supported.

## Use it with AI agents (MCP)

`slimdb mcp` runs an MCP server on stdio with these tools:

| Tool | What it does |
| --- | --- |
| `list_connections` | Saved connections (names and kinds, never URLs or passwords) |
| `add_connection` | Save a new connection after testing it |
| `list_objects` | Schemas and tables, or databases and collections |
| `describe` | Columns, indexes, constraints, or inferred Mongo fields |
| `query` | Run SQL or mongosh statements (default 100 rows, max 1000) |
| `explain` | Query plan, optionally with analyze |

**Claude Code:**

```sh
claude mcp add slimdb -- slimdb mcp
```

**Claude Desktop, Cursor and other clients** (JSON config):

```json
{
  "mcpServers": {
    "slimdb": { "command": "slimdb", "args": ["mcp"] }
  }
}
```

Use the full path to the binary if it is not on the client's `PATH`. With the
Docker image, use `docker` as the command and `["exec", "-i", "slimdb", "slimdb", "mcp"]`
as the arguments; the agent then shares the container's saved connections.

> **Agents can write.** The `query` tool runs whatever it is given, including
> `UPDATE`, `DELETE` and `drop()`. Mark a connection read-only (in the UI or with
> `conn add -read-only`) for anything you do not want an agent to change. For a
> hard guarantee, connect with a database user that only has read access.

## Where things are stored

Connections and history live in your user config folder, `slimdb/`:

- macOS: `~/Library/Application Support/slimdb`
- Linux: `~/.config/slimdb`
- Windows: `%AppData%\slimdb`

Set `SLIMDB_HOME` to use another folder. Connection URLs, passwords included, are
stored in plain text in `connections.json` with file mode 0600.

## Security notes

- The web UI only accepts requests for `localhost`/loopback hosts, which blocks
  DNS-rebinding attacks, and every API call needs a random token issued per run,
  which blocks other websites from calling the API (CSRF).
- `slimdb serve` refuses to listen on a non-loopback address.
- The read-only flag sets `default_transaction_read_only` on Postgres and refuses
  write methods on MongoDB. It guards against mistakes; it is not a security
  boundary.

## Development

```sh
go test ./...                                   # unit tests

# integration tests need real databases
docker run -d --name pg -e POSTGRES_PASSWORD=pass -p 5432:5432 postgres:17
docker run -d --name mongo -p 27017:27017 mongo:7
SLIMDB_TEST_POSTGRES_URL=postgres://postgres:pass@localhost:5432/postgres \
SLIMDB_TEST_MONGO_URL=mongodb://localhost:27017 \
go test ./...

go run ./cmd/slimdb                             # run from source
```

Layout:

```
cmd/slimdb          CLI: serve, mcp, conn
internal/store      saved connections and history on disk
internal/db         Driver interface, Postgres and MongoDB drivers, SQL splitter, mongosh parser
internal/web        HTTP API and the embedded UI (plain HTML, CSS and JS, no build step)
internal/mcpserver  MCP tools
```

Issues and pull requests are welcome. Please run `gofmt` and the tests before
opening a PR.

## License

[MIT](LICENSE)
