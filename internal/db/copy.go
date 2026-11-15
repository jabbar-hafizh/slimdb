package db

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"

	"github.com/jabbar-hafizh/slimdb/internal/store"
)

// Copy modes.
const (
	CopyAll    = "all"    // schema and data, replacing what the target has
	CopySchema = "schema" // tables, indexes and the rest, no rows
	CopyData   = "data"   // rows only, replacing the rows of tables that already exist
)

type CopyOptions struct {
	Mode string
	// Schemas limits a Postgres copy to these schemas; empty means all.
	Schemas []string
}

// CopyReport says how a finished copy went. Warnings are statements the
// target refused, e.g. an extension that only exists on the source server.
type CopyReport struct {
	Warnings []string
}

// CheckCopy returns why src cannot be copied into dst, or nil.
func CheckCopy(src, dst store.Conn, opt CopyOptions) error {
	switch {
	case src.ID != "" && src.ID == dst.ID:
		return errors.New("source and target are the same connection")
	case src.Kind != dst.Kind:
		return fmt.Errorf("cannot copy %s into %s", src.Kind, dst.Kind)
	case src.Kind != store.KindPostgres && src.Kind != store.KindMongo:
		return fmt.Errorf("copying is only available for Postgres and MongoDB, not %s", src.Kind)
	case dst.ReadOnly:
		return fmt.Errorf("target %q is read-only", dst.Name)
	}
	switch opt.Mode {
	case CopyAll, CopySchema, CopyData:
	default:
		return fmt.Errorf("unknown mode %q (use all, schema or data)", opt.Mode)
	}
	return nil
}

// IsLocalTarget reports whether the connection points at this machine, so
// the UI and CLI can ask for confirmation before overwriting anything else.
func IsLocalTarget(c store.Conn) bool {
	u, err := url.Parse(c.URL)
	if err != nil {
		return false
	}
	hosts := strings.Split(u.Host, ",")
	if u.Host == "" {
		hosts = []string{"localhost"} // unix socket or libpq default
	}
	for _, h := range hosts {
		if hh, _, err := net.SplitHostPort(h); err == nil {
			h = hh
		}
		h = strings.Trim(strings.ToLower(h), "[]")
		ip := net.ParseIP(h)
		if !(h == "" || h == "localhost" || h == "host.docker.internal" || (ip != nil && ip.IsLoopback())) {
			return false
		}
	}
	return true
}

// Copy copies a whole database from src into dst. Postgres goes through
// pg_dump and pg_restore, which must be on PATH (the Docker image has them);
// MongoDB is copied with the driver. logf receives progress lines.
func Copy(ctx context.Context, src, dst store.Conn, opt CopyOptions, logf func(string)) (CopyReport, error) {
	if err := CheckCopy(src, dst, opt); err != nil {
		return CopyReport{}, err
	}
	alias := os.Getenv("SLIMDB_LOCALHOST_ALIAS")
	srcURL, dstURL := rewriteLocalhost(src.URL, alias), rewriteLocalhost(dst.URL, alias)
	if src.Kind == store.KindMongo {
		return copyMongo(ctx, srcURL, dstURL, opt, logf)
	}
	return copyPostgres(ctx, srcURL, dstURL, opt, logf)
}

// ---------- Postgres ----------

// libpq tools take the URL without the password, which goes through
// PGPASSWORD so it does not show up in the process list.
func libpqTarget(raw string) (uri, password string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	if u.User != nil {
		password, _ = u.User.Password()
		u.User = url.User(u.User.Username())
	}
	return u.String(), password, nil
}

func pgCommand(ctx context.Context, name, password string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+password, "PGCONNECT_TIMEOUT=15")
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func copyPostgres(ctx context.Context, srcURL, dstURL string, opt CopyOptions, logf func(string)) (CopyReport, error) {
	for _, tool := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(tool); err != nil {
			return CopyReport{}, fmt.Errorf("%s not found: install the PostgreSQL client tools, or use the Docker image, which has them", tool)
		}
	}
	src, srcPass, err := libpqTarget(srcURL)
	if err != nil {
		return CopyReport{}, err
	}
	dst, dstPass, err := libpqTarget(dstURL)
	if err != nil {
		return CopyReport{}, err
	}
	if err := ensurePgDatabase(ctx, dstURL, logf); err != nil {
		return CopyReport{}, err
	}
	if opt.Mode == CopyData {
		if err := truncateTargetTables(ctx, srcURL, dstURL, opt.Schemas, logf); err != nil {
			return CopyReport{}, err
		}
	}

	common := []string{"--no-owner", "--no-privileges"}
	dumpArgs := append([]string{"--format=custom", "--verbose", "--dbname=" + src}, common...)
	restoreArgs := append([]string{"--verbose", "--dbname=" + dst}, common...)
	switch opt.Mode {
	case CopyAll:
		restoreArgs = append(restoreArgs, "--clean", "--if-exists")
	case CopySchema:
		dumpArgs = append(dumpArgs, "--schema-only")
		restoreArgs = append(restoreArgs, "--clean", "--if-exists")
	case CopyData:
		dumpArgs = append(dumpArgs, "--data-only")
		// Rows arrive table by table, so foreign keys would fail on order.
		restoreArgs = append(restoreArgs, "--disable-triggers")
	}
	for _, s := range opt.Schemas {
		if s = strings.TrimSpace(s); s != "" {
			dumpArgs = append(dumpArgs, "--schema="+s)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dump := pgCommand(ctx, "pg_dump", srcPass, dumpArgs...)
	restore := pgCommand(ctx, "pg_restore", dstPass, restoreArgs...)
	pipe, err := dump.StdoutPipe()
	if err != nil {
		return CopyReport{}, err
	}
	restore.Stdin = pipe

	var report CopyReport
	var mu sync.Mutex
	var wg sync.WaitGroup
	follow := func(prefix string, r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimPrefix(sc.Text(), prefix+": ")
			if isPgWarning(line) {
				mu.Lock()
				report.Warnings = append(report.Warnings, line)
				mu.Unlock()
			}
			logf(prefix + ": " + line)
		}
	}
	dumpErr, _ := dump.StderrPipe()
	restoreErr, _ := restore.StderrPipe()
	wg.Add(2)
	go follow("pg_dump", dumpErr)
	go follow("pg_restore", restoreErr)

	logf("starting pg_dump and pg_restore")
	if err := dump.Start(); err != nil {
		return report, err
	}
	if err := restore.Start(); err != nil {
		cancel()
		_ = dump.Wait()
		return report, err
	}
	restoreWait := restore.Wait()
	dumpWait := dump.Wait()
	wg.Wait()

	if ctx.Err() != nil {
		return report, ctx.Err()
	}
	if dumpWait != nil {
		return report, fmt.Errorf("pg_dump failed: %w (see the log)", dumpWait)
	}
	// pg_restore exits 1 when it skipped statements the target refused
	// ("errors ignored on restore"); everything else went in.
	var exit *exec.ExitError
	if restoreWait != nil && !(errors.As(restoreWait, &exit) && exit.ExitCode() == 1 && len(report.Warnings) > 0) {
		return report, fmt.Errorf("pg_restore failed: %w (see the log)", restoreWait)
	}
	return report, nil
}

// One "error: ..." line per statement the target refused.
var pgWarningRE = regexp.MustCompile(`^error: `)

func isPgWarning(line string) bool { return pgWarningRE.MatchString(line) }

// truncateTargetTables empties the target's copies of the source tables, so
// a data-only copy replaces rows instead of colliding with them. Tables
// owned by an extension (e.g. PostGIS's spatial_ref_sys) are left alone.
func truncateTargetTables(ctx context.Context, srcURL, dstURL string, schemas []string, logf func(string)) error {
	src, err := pgx.Connect(ctx, srcURL)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	defer src.Close(ctx)
	want := []string{} // not nil: pgx sends a nil slice as NULL
	for _, s := range schemas {
		if s = strings.TrimSpace(s); s != "" {
			want = append(want, s)
		}
	}
	rows, err := src.Query(ctx, `
		select format('%I.%I', n.nspname, c.relname)
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where c.relkind in ('r', 'p')
			and n.nspname not in ('pg_catalog', 'information_schema') and n.nspname not like 'pg_toast%'
			and (cardinality($1::text[]) = 0 or n.nspname = any($1::text[]))
			and not exists (select 1 from pg_depend d where d.classid = 'pg_class'::regclass
				and d.objid = c.oid and d.deptype = 'e')`, want)
	if err != nil {
		return fmt.Errorf("list source tables: %w", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}

	dst, err := pgx.Connect(ctx, dstURL)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	defer dst.Close(ctx)
	var existing []string
	for _, t := range tables {
		var ok bool
		if err := dst.QueryRow(ctx, `select to_regclass($1) is not null`, t).Scan(&ok); err != nil {
			return err
		}
		if ok {
			existing = append(existing, t)
		}
	}
	if len(existing) == 0 {
		return nil
	}
	if _, err := dst.Exec(ctx, "truncate table "+strings.Join(existing, ", ")); err != nil {
		return fmt.Errorf("empty target tables: %w", err)
	}
	logf(fmt.Sprintf("emptied %d target tables", len(existing)))
	return nil
}

// ensurePgDatabase creates the target database when it does not exist yet,
// so copying into a fresh local server needs no manual step.
func ensurePgDatabase(ctx context.Context, rawURL string, logf func(string)) error {
	cfg, err := pgx.ParseConfig(rawURL)
	if err != nil {
		return err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err == nil {
		return conn.Close(ctx)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "3D000" { // invalid_catalog_name
		return fmt.Errorf("target: %w", err)
	}
	name := cfg.Database
	admin := cfg.Copy()
	admin.Database = "postgres"
	conn, err = pgx.ConnectConfig(ctx, admin)
	if err != nil {
		return fmt.Errorf("target database %q does not exist and the server could not be reached to create it: %w", name, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "create database "+pgx.Identifier{name}.Sanitize()); err != nil {
		return fmt.Errorf("create database %q: %w", name, err)
	}
	logf(fmt.Sprintf("created database %q on the target", name))
	return nil
}

// ---------- MongoDB ----------

const mongoBatch = 1000

func copyMongo(ctx context.Context, srcURL, dstURL string, opt CopyOptions, logf func(string)) (CopyReport, error) {
	var report CopyReport
	srcCS, err := connstring.ParseAndValidate(srcURL)
	if err != nil {
		return report, err
	}
	if srcCS.Database == "" {
		return report, errors.New("put the source database in its URL, e.g. mongodb://host:27017/shop")
	}
	dstCS, err := connstring.ParseAndValidate(dstURL)
	if err != nil {
		return report, err
	}
	dstName := dstCS.Database
	if dstName == "" {
		dstName = srcCS.Database
	}

	srcClient, err := mongo.Connect(options.Client().ApplyURI(srcURL).SetAppName("slimdb"))
	if err != nil {
		return report, err
	}
	defer srcClient.Disconnect(context.Background())
	dstClient, err := mongo.Connect(options.Client().ApplyURI(dstURL).SetAppName("slimdb"))
	if err != nil {
		return report, err
	}
	defer dstClient.Disconnect(context.Background())

	srcDB := srcClient.Database(srcCS.Database)
	dstDB := dstClient.Database(dstName)
	specs, err := srcDB.ListCollectionSpecifications(ctx, bson.D{})
	if err != nil {
		return report, fmt.Errorf("list source collections: %w", err)
	}
	logf(fmt.Sprintf("copying %d collections from %s into %s", len(specs), srcCS.Database, dstName))

	for _, spec := range specs {
		if strings.HasPrefix(spec.Name, "system.") {
			continue
		}
		if spec.Type == "view" {
			warn := fmt.Sprintf("skipped view %s: views are not copied", spec.Name)
			report.Warnings = append(report.Warnings, warn)
			logf(warn)
			continue
		}
		if err := copyCollection(ctx, srcDB, dstDB, spec.Name, opt.Mode, &report, logf); err != nil {
			return report, fmt.Errorf("%s: %w", spec.Name, err)
		}
	}
	return report, nil
}

func copyCollection(ctx context.Context, srcDB, dstDB *mongo.Database, name, mode string, report *CopyReport, logf func(string)) error {
	src, dst := srcDB.Collection(name), dstDB.Collection(name)
	if mode != CopyData {
		if err := dst.Drop(ctx); err != nil {
			return err
		}
		if err := dstDB.CreateCollection(ctx, name); err != nil {
			return err
		}
	} else if _, err := dst.DeleteMany(ctx, bson.D{}); err != nil {
		return err
	}

	copied := 0
	if mode != CopySchema {
		cur, err := src.Find(ctx, bson.D{})
		if err != nil {
			return err
		}
		defer cur.Close(ctx)
		batch := make([]any, 0, mongoBatch)
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			_, err := dst.InsertMany(ctx, batch, options.InsertMany().SetOrdered(false))
			var bulk mongo.BulkWriteException
			if errors.As(err, &bulk) {
				// E.g. a document the target's own unique index refuses.
				warn := fmt.Sprintf("%s: %d documents skipped (%s)", name, len(bulk.WriteErrors), bulk.WriteErrors[0].Message)
				report.Warnings = append(report.Warnings, warn)
				logf(warn)
				err = nil
			}
			copied += len(batch)
			batch = batch[:0]
			return err
		}
		for cur.Next(ctx) {
			batch = append(batch, bson.Raw(append([]byte(nil), cur.Current...)))
			if len(batch) == mongoBatch {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if err := cur.Err(); err != nil {
			return err
		}
		if err := flush(); err != nil {
			return err
		}
	}

	indexes := 0
	if mode != CopyData {
		cur, err := src.Indexes().List(ctx)
		if err != nil {
			return err
		}
		var specs []bson.D
		if err := cur.All(ctx, &specs); err != nil {
			return err
		}
		for _, spec := range specs {
			model, ok := indexModel(spec)
			if !ok {
				continue
			}
			if _, err := dst.Indexes().CreateOne(ctx, model); err != nil {
				warn := fmt.Sprintf("%s: index not created: %v", name, err)
				report.Warnings = append(report.Warnings, warn)
				logf(warn)
				continue
			}
			indexes++
		}
	}
	logf(fmt.Sprintf("%s: %d documents, %d indexes", name, copied, indexes))
	return nil
}

// indexModel turns a listIndexes entry back into a create request. The _id
// index exists already.
func indexModel(spec bson.D) (mongo.IndexModel, bool) {
	var keys any
	opts := bson.D{}
	for _, e := range spec {
		switch e.Key {
		case "key":
			keys = e.Value
		case "v", "ns":
		default:
			opts = append(opts, e)
		}
	}
	for _, e := range opts {
		if e.Key == "name" && e.Value == "_id_" {
			return mongo.IndexModel{}, false
		}
	}
	model := mongo.IndexModel{Keys: keys, Options: options.Index()}
	for _, e := range opts {
		switch e.Key {
		case "name":
			model.Options.SetName(fmt.Sprint(e.Value))
		case "unique":
			model.Options.SetUnique(e.Value == true)
		case "sparse":
			model.Options.SetSparse(e.Value == true)
		case "expireAfterSeconds":
			if n, ok := toInt32(e.Value); ok {
				model.Options.SetExpireAfterSeconds(n)
			}
		case "partialFilterExpression":
			model.Options.SetPartialFilterExpression(e.Value)
		case "collation":
			if d, ok := e.Value.(bson.D); ok {
				var c options.Collation
				if raw, err := bson.Marshal(d); err == nil && bson.Unmarshal(raw, &c) == nil {
					model.Options.SetCollation(&c)
				}
			}
		case "weights":
			model.Options.SetWeights(e.Value)
		case "default_language":
			model.Options.SetDefaultLanguage(fmt.Sprint(e.Value))
		case "2dsphereIndexVersion":
			if n, ok := toInt32(e.Value); ok {
				model.Options.SetSphereVersion(n)
			}
		}
	}
	return model, true
}

func toInt32(v any) (int32, bool) {
	switch n := v.(type) {
	case int32:
		return n, true
	case int64:
		return int32(n), true
	case float64:
		return int32(n), true
	}
	return 0, false
}
