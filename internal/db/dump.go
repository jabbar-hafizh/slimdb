package db

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"

	"github.com/jabbar-hafizh/slimdb/internal/store"
)

// Dump files. Postgres dumps are pg_dump's custom format, so pg_restore can
// read them too. MongoDB dumps are slimdb's own: a gzipped stream of BSON
// documents, a header first, then per collection its indexes and documents.

const mongoDumpMagic = "slimdb-mongo-dump"

// CheckDump returns why c cannot be dumped, or nil.
func CheckDump(c store.Conn, opt CopyOptions) error {
	if c.Kind != store.KindPostgres && c.Kind != store.KindMongo {
		return fmt.Errorf("dump and import support Postgres and MongoDB, not %s", c.Kind)
	}
	switch opt.Mode {
	case CopyAll, CopySchema, CopyData:
	default:
		return fmt.Errorf("unknown mode %q (use all, schema or data)", opt.Mode)
	}
	if c.Kind == store.KindPostgres {
		if _, err := exec.LookPath("pg_dump"); err != nil {
			return errors.New("pg_dump not found: install the PostgreSQL client tools, or use the Docker image, which has them")
		}
	}
	return nil
}

// DumpFileName suggests a file name like "li3-dev-app-data-20261115-1304.dump".
func DumpFileName(c store.Conn, mode string, now time.Time) string {
	name := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(c.Name, "-")
	name = strings.Trim(name, "-.")
	if name == "" {
		name = "slimdb"
	}
	if mode != "" && mode != CopyAll {
		name += "-" + mode
	}
	name += "-" + now.Format("20060102-1504")
	if c.Kind == store.KindMongo {
		return name + ".mongo.gz"
	}
	return name + ".dump"
}

// Dump writes c's database to w. logf receives progress lines.
func Dump(ctx context.Context, c store.Conn, opt CopyOptions, w io.Writer, logf func(string)) error {
	if err := CheckDump(c, opt); err != nil {
		return err
	}
	u := rewriteLocalhost(c.URL, os.Getenv("SLIMDB_LOCALHOST_ALIAS"))
	if c.Kind == store.KindMongo {
		return dumpMongo(ctx, u, opt, w, logf)
	}
	src, pass, err := libpqTarget(u)
	if err != nil {
		return err
	}
	args := []string{"--format=custom", "--verbose", "--no-owner", "--no-privileges", "--dbname=" + src}
	switch opt.Mode {
	case CopySchema:
		args = append(args, "--schema-only")
	case CopyData:
		args = append(args, "--data-only")
	}
	for _, s := range opt.Schemas {
		if s = strings.TrimSpace(s); s != "" {
			args = append(args, "--schema="+s)
		}
	}
	cmd := pgCommand(ctx, "pg_dump", pass, args...)
	cmd.Stdout = w
	var report CopyReport
	return runPgTool(cmd, "pg_dump", &report, logf)
}

// runPgTool runs a libpq tool, sends its stderr to logf and collects the
// statements it skipped as warnings. Exit code 1 with warnings is how
// pg_restore and psql say "done, but some statements failed".
func runPgTool(cmd *exec.Cmd, prefix string, report *CopyReport, logf func(string)) error {
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), prefix+": ")
		if isPgWarning(line) || strings.Contains(line, "ERROR:  ") {

			report.Warnings = append(report.Warnings, line)

		}
		logf(prefix + ": " + line)
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	if err != nil && errors.As(err, &exit) && exit.ExitCode() == 1 && len(report.Warnings) > 0 && prefix != "pg_dump" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s failed: %w (see the log)", prefix, err)
	}
	return nil
}

// Restore loads a dump file into c, replacing what the file contains, like
// Copy does. Postgres takes pg_dump custom-format files and plain SQL files.
func Restore(ctx context.Context, c store.Conn, opt CopyOptions, path string, logf func(string)) (CopyReport, error) {
	var report CopyReport
	if c.ReadOnly {
		return report, fmt.Errorf("%q is read-only", c.Name)
	}
	if c.Kind != store.KindPostgres && c.Kind != store.KindMongo {
		return report, fmt.Errorf("dump and import support Postgres and MongoDB, not %s", c.Kind)
	}
	switch opt.Mode {
	case CopyAll, CopySchema, CopyData:
	default:
		return report, fmt.Errorf("unknown mode %q (use all, schema or data)", opt.Mode)
	}
	head := make([]byte, 5)
	f, err := os.Open(path)
	if err != nil {
		return report, err
	}
	n, _ := io.ReadFull(f, head)
	f.Close()
	head = head[:n]

	u := rewriteLocalhost(c.URL, os.Getenv("SLIMDB_LOCALHOST_ALIAS"))
	if c.Kind == store.KindMongo {
		if !bytes.HasPrefix(head, []byte{0x1f, 0x8b}) {
			return report, errors.New("not a slimdb MongoDB dump (expected a .mongo.gz file made by slimdb)")
		}
		return restoreMongo(ctx, u, opt, path, logf)
	}
	if err := ensurePgDatabase(ctx, u, logf); err != nil {
		return report, err
	}
	dst, pass, err := libpqTarget(u)
	if err != nil {
		return report, err
	}
	if !bytes.Equal(head, []byte("PGDMP")) {
		if bytes.HasPrefix(head, []byte{0x1f, 0x8b}) {
			return report, errors.New("this looks like a MongoDB dump or a gzipped file; unzip a .sql.gz first")
		}
		if opt.Mode != CopyAll {
			logf("plain SQL file: the mode does not apply, the file runs as it is")
		}
		if _, err := exec.LookPath("psql"); err != nil {
			return report, errors.New("psql not found: install the PostgreSQL client tools, or use the Docker image, which has them")
		}
		logf("running the SQL file with psql")
		cmd := pgCommand(ctx, "psql", pass, "-X", "-q", "-v", "ON_ERROR_STOP=0", "--dbname="+dst, "-f", path)
		cmd.Stdout = io.Discard
		err := runPgTool(cmd, "psql", &report, logf)
		return report, err
	}

	if _, err := exec.LookPath("pg_restore"); err != nil {
		return report, errors.New("pg_restore not found: install the PostgreSQL client tools, or use the Docker image, which has them")
	}
	args := []string{"--verbose", "--no-owner", "--no-privileges", "--dbname=" + dst}
	switch opt.Mode {
	case CopyAll:
		args = append(args, "--clean", "--if-exists")
	case CopySchema:
		args = append(args, "--schema-only", "--clean", "--if-exists")
	case CopyData:
		args = append(args, "--data-only", "--disable-triggers")
		tables, err := dumpTables(ctx, path)
		if err != nil {
			return report, err
		}
		if err := truncateExisting(ctx, u, tables, logf); err != nil {
			return report, err
		}
	}
	for _, s := range opt.Schemas {
		if s = strings.TrimSpace(s); s != "" {
			args = append(args, "--schema="+s)
		}
	}
	args = append(args, path)
	logf("restoring with pg_restore")
	err = runPgTool(pgCommand(ctx, "pg_restore", pass, args...), "pg_restore", &report, logf)
	return report, err
}

// dumpTables lists the tables whose rows a custom-format dump holds, from
// its table of contents, e.g. "4279; 0 20820 TABLE DATA ev ev_site postgres".
func dumpTables(ctx context.Context, path string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "pg_restore", "--list", path).Output()
	if err != nil {
		return nil, fmt.Errorf("read the dump's contents: %w", err)
	}
	var tables []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, ";") {
			continue
		}
		_, rest, ok := strings.Cut(line, " TABLE DATA ")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) >= 2 {
			tables = append(tables, pgx.Identifier{f[0], f[1]}.Sanitize())
		}
	}
	return tables, nil
}

// truncateExisting empties the listed tables that exist in the target.
func truncateExisting(ctx context.Context, dstURL string, tables []string, logf func(string)) error {
	if len(tables) == 0 {
		return nil
	}
	conn, err := pgx.Connect(ctx, dstURL)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	defer conn.Close(ctx)
	var existing []string
	for _, t := range tables {
		var ok bool
		if err := conn.QueryRow(ctx, `select to_regclass($1) is not null`, t).Scan(&ok); err != nil {
			return err
		}
		if ok {
			existing = append(existing, t)
		}
	}
	if len(existing) == 0 {
		return nil
	}
	if _, err := conn.Exec(ctx, "truncate table "+strings.Join(existing, ", ")); err != nil {
		return fmt.Errorf("empty target tables: %w", err)
	}
	logf(fmt.Sprintf("emptied %d target tables", len(existing)))
	return nil
}

// ---------- MongoDB ----------

type mongoDumpHeader struct {
	Magic    string `bson:"magic"`
	Version  int    `bson:"version"`
	Database string `bson:"database"`
	Mode     string `bson:"mode"`
}

// A record is either a collection start (Collection set) or one document.
type mongoDumpRecord struct {
	Collection string   `bson:"collection,omitempty"`
	Indexes    []bson.D `bson:"indexes,omitempty"`
	Doc        bson.Raw `bson:"doc,omitempty"`
}

func dumpMongo(ctx context.Context, rawURL string, opt CopyOptions, w io.Writer, logf func(string)) error {
	cs, err := connstring.ParseAndValidate(rawURL)
	if err != nil {
		return err
	}
	if cs.Database == "" {
		return errors.New("put the database in the connection URL, e.g. mongodb://host:27017/shop")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(rawURL).SetAppName("slimdb"))
	if err != nil {
		return err
	}
	defer client.Disconnect(context.Background())
	db := client.Database(cs.Database)
	specs, err := db.ListCollectionSpecifications(ctx, bson.D{})
	if err != nil {
		return err
	}

	gz := gzip.NewWriter(w)
	write := func(v any) error {
		b, err := bson.Marshal(v)
		if err != nil {
			return err
		}
		_, err = gz.Write(b)
		return err
	}
	if err := write(mongoDumpHeader{Magic: mongoDumpMagic, Version: 1, Database: cs.Database, Mode: opt.Mode}); err != nil {
		return err
	}
	for _, spec := range specs {
		if strings.HasPrefix(spec.Name, "system.") || spec.Type == "view" {
			if spec.Type == "view" {
				logf(fmt.Sprintf("skipped view %s: views are not dumped", spec.Name))
			}
			continue
		}
		coll := db.Collection(spec.Name)
		rec := mongoDumpRecord{Collection: spec.Name}
		if opt.Mode != CopyData {
			cur, err := coll.Indexes().List(ctx)
			if err != nil {
				return err
			}
			if err := cur.All(ctx, &rec.Indexes); err != nil {
				return err
			}
		}
		if err := write(rec); err != nil {
			return err
		}
		n := 0
		if opt.Mode != CopySchema {
			cur, err := coll.Find(ctx, bson.D{})
			if err != nil {
				return err
			}
			for cur.Next(ctx) {
				if err := write(mongoDumpRecord{Doc: cur.Current}); err != nil {
					cur.Close(ctx)
					return err
				}
				n++
			}
			err = cur.Err()
			cur.Close(ctx)
			if err != nil {
				return err
			}
		}
		logf(fmt.Sprintf("%s: %d documents, %d indexes", spec.Name, n, len(rec.Indexes)))
	}
	return gz.Close()
}

func restoreMongo(ctx context.Context, rawURL string, opt CopyOptions, path string, logf func(string)) (CopyReport, error) {
	var report CopyReport
	f, err := os.Open(path)
	if err != nil {
		return report, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return report, err
	}
	br := bufio.NewReaderSize(gz, 1<<20)
	next := func() (bson.Raw, error) {
		var size [4]byte
		if _, err := io.ReadFull(br, size[:]); err != nil {
			return nil, err
		}
		n := int(uint32(size[0]) | uint32(size[1])<<8 | uint32(size[2])<<16 | uint32(size[3])<<24)
		if n < 5 || n > 64<<20 {
			return nil, errors.New("the dump file is damaged")
		}
		doc := make([]byte, n)
		copy(doc, size[:])
		if _, err := io.ReadFull(br, doc[4:]); err != nil {
			return nil, err
		}
		return doc, nil
	}

	raw, err := next()
	if err != nil {
		return report, errors.New("not a slimdb MongoDB dump")
	}
	var head mongoDumpHeader
	if bson.Unmarshal(raw, &head) != nil || head.Magic != mongoDumpMagic {
		return report, errors.New("not a slimdb MongoDB dump")
	}
	cs, err := connstring.ParseAndValidate(rawURL)
	if err != nil {
		return report, err
	}
	name := cs.Database
	if name == "" {
		name = head.Database
	}
	client, err := mongo.Connect(options.Client().ApplyURI(rawURL).SetAppName("slimdb"))
	if err != nil {
		return report, err
	}
	defer client.Disconnect(context.Background())
	db := client.Database(name)
	logf(fmt.Sprintf("importing a dump of %s (%s) into %s", head.Database, head.Mode, name))

	var coll *mongo.Collection
	var collName string
	var indexes []bson.D
	batch := make([]any, 0, mongoBatch)
	count := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, err := coll.InsertMany(ctx, batch, options.InsertMany().SetOrdered(false))
		var bulk mongo.BulkWriteException
		if errors.As(err, &bulk) {
			warn := fmt.Sprintf("%s: %d documents skipped (%s)", collName, len(bulk.WriteErrors), bulk.WriteErrors[0].Message)
			report.Warnings = append(report.Warnings, warn)
			logf(warn)
			err = nil
		}
		count += len(batch)
		batch = batch[:0]
		return err
	}
	finish := func() error {
		if coll == nil {
			return nil
		}
		if err := flush(); err != nil {
			return err
		}
		made := 0
		if opt.Mode != CopyData {
			for _, spec := range indexes {
				model, ok := indexModel(spec)
				if !ok {
					continue
				}
				if _, err := coll.Indexes().CreateOne(ctx, model); err != nil {
					warn := fmt.Sprintf("%s: index not created: %v", collName, err)
					report.Warnings = append(report.Warnings, warn)
					logf(warn)
					continue
				}
				made++
			}
		}
		logf(fmt.Sprintf("%s: %d documents, %d indexes", collName, count, made))
		return nil
	}

	for {
		raw, err := next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return report, fmt.Errorf("read the dump: %w", err)
		}
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		if doc, ok := raw.Lookup("doc").DocumentOK(); ok {
			if coll == nil || opt.Mode == CopySchema {
				continue
			}
			batch = append(batch, doc)
			if len(batch) == mongoBatch {
				if err := flush(); err != nil {
					return report, err
				}
			}
			continue
		}
		var rec mongoDumpRecord
		if err := bson.Unmarshal(raw, &rec); err != nil || rec.Collection == "" {
			return report, errors.New("the dump file is damaged")
		}
		if err := finish(); err != nil {
			return report, err
		}
		collName, indexes, count = rec.Collection, rec.Indexes, 0
		coll = db.Collection(collName)
		if opt.Mode != CopyData {
			if err := coll.Drop(ctx); err != nil {
				return report, err
			}
			if err := db.CreateCollection(ctx, collName); err != nil {
				return report, err
			}
		} else if _, err := coll.DeleteMany(ctx, bson.D{}); err != nil {
			return report, err
		}
	}
	return report, finish()
}
