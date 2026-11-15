// Command slimdb is a small local database client for PostgreSQL, MySQL,
// SQLite, SQL Server, MongoDB and Redis, with a browser UI and an MCP server
// for AI agents.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jabbar-hafizh/slimdb/internal/db"
	"github.com/jabbar-hafizh/slimdb/internal/mcpserver"
	"github.com/jabbar-hafizh/slimdb/internal/store"
	"github.com/jabbar-hafizh/slimdb/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `slimdb: a slim database client for PostgreSQL, MySQL, SQLite, SQL Server, MongoDB and Redis

Usage:
  slimdb [serve] [-addr 127.0.0.1:7788] [-no-browser]   start the web UI
  slimdb mcp                                          run the MCP server on stdio
  slimdb conn list                                    list saved connections
  slimdb conn add [-read-only] <name> <url>           save a connection
  slimdb conn rename <name> <new-name>                rename a connection
  slimdb conn rm <name>                               delete a connection
  slimdb copy [-mode all|schema|data] [-schema a,b] [-yes] <source> <target>
                                                      copy a database into another connection
  slimdb dump [-mode all|schema|data] [-schema a,b] <conn> [file]
                                                      dump a database to a file (default: a dated name here)
  slimdb import [-mode all|schema|data] [-schema a,b] [-yes] <file> <conn>
                                                      load a dump file into a connection
  slimdb version

Connections and history are stored in %s
(override with SLIMDB_HOME).
`

func main() {
	log.SetFlags(0)
	dir, err := store.DefaultDir()
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		log.Fatal(err)
	}

	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		err = serve(ctx, st, args)
	case "mcp":
		dbs := db.NewManager()
		defer dbs.CloseAll()
		err = mcpserver.Run(ctx, st, dbs, version)
	case "conn":
		err = connCmd(st, args)
	case "copy":
		err = copyCmd(ctx, st, args)
	case "dump":
		err = dumpCmd(ctx, st, args)
	case "import":
		err = importCmd(ctx, st, args)
	case "version", "-v", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Printf(usage, dir)
	default:
		fmt.Fprintf(os.Stderr, usage, dir)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func serve(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7788", "listen address (keep it on localhost)")
	noBrowser := fs.Bool("no-browser", false, "do not open the browser")
	_ = fs.Parse(args)

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	// Inside a container the server must listen on all interfaces; the
	// Docker image opts in, and the port should be published on 127.0.0.1.
	listenAny := os.Getenv("SLIMDB_LISTEN_ANY") == "1"
	if ip := net.ParseIP(host); !listenAny && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("refusing to listen on %s: slimdb runs queries for whoever can reach it, keep it on localhost", *addr)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		// The default port is taken (maybe by another slimdb): pick a free one.
		ln, err = net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			return err
		}
	}

	dbs := db.NewManager()
	defer dbs.CloseAll()
	srv := &http.Server{Handler: web.New(st, dbs).Handler(), ReadHeaderTimeout: 10 * time.Second}

	port := ln.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://%s", net.JoinHostPort(displayHost(host), strconv.Itoa(port)))
	fmt.Printf("slimdb %s running at %s (Ctrl+C to stop)\n", version, url)
	if !*noBrowser {
		openBrowser(url)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func connCmd(st *store.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: slimdb conn list | add [-read-only] <name> <url> | rename <name> <new-name> | rm <name>")
	}
	switch args[0] {
	case "list", "ls":
		conns, err := st.Conns()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tKIND\tREAD-ONLY\tID")
		for _, c := range conns {
			fmt.Fprintf(w, "%s\t%s\t%v\t%s\n", c.Name, c.Kind, c.ReadOnly, c.ID)
		}
		return w.Flush()
	case "add":
		fs := flag.NewFlagSet("conn add", flag.ExitOnError)
		readOnly := fs.Bool("read-only", false, "refuse writes on this connection")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 2 {
			return errors.New("usage: slimdb conn add [-read-only] <name> <url>")
		}
		c, err := st.Save(store.Conn{Name: fs.Arg(0), URL: fs.Arg(1), ReadOnly: *readOnly})
		if err != nil {
			return err
		}
		fmt.Printf("saved %s connection %q\n", c.Kind, c.Name)
		return nil
	case "rename", "mv":
		if len(args) != 3 {
			return errors.New("usage: slimdb conn rename <name> <new-name>")
		}
		c, err := st.Conn(args[1])
		if err != nil {
			return err
		}
		c.Name = args[2]
		if _, err := st.Save(c); err != nil {
			return err
		}
		fmt.Printf("renamed %q to %q\n", args[1], c.Name)
		return nil
	case "rm", "remove", "delete":
		if len(args) != 2 {
			return errors.New("usage: slimdb conn rm <name>")
		}
		c, err := st.Conn(args[1])
		if err != nil {
			return err
		}
		if err := st.Delete(c.ID); err != nil {
			return err
		}
		fmt.Printf("deleted %q\n", c.Name)
		return nil
	}
	return fmt.Errorf("unknown conn command %q", args[0])
}

func copyCmd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("copy", flag.ExitOnError)
	mode := fs.String("mode", db.CopyAll, "all (schema and data, replaces the target's objects), schema, or data (rows into existing tables)")
	schemas := fs.String("schema", "", "Postgres only: comma-separated schemas to copy (default all)")
	yes := fs.Bool("yes", false, "allow a target that is not on this machine")
	_ = fs.Parse(args)
	if fs.NArg() != 2 {
		return errors.New("usage: slimdb copy [-mode all|schema|data] [-schema a,b] [-yes] <source> <target>")
	}
	src, err := st.Conn(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("source %q: %w", fs.Arg(0), err)
	}
	dst, err := st.Conn(fs.Arg(1))
	if err != nil {
		return fmt.Errorf("target %q: %w", fs.Arg(1), err)
	}
	if !db.IsLocalTarget(dst) && !*yes {
		return fmt.Errorf("%q is not on this machine; pass -yes to overwrite it anyway", dst.Name)
	}
	opt := db.CopyOptions{Mode: *mode}
	if *schemas != "" {
		opt.Schemas = strings.Split(*schemas, ",")
	}
	start := time.Now()
	report, err := db.Copy(ctx, src, dst, opt, func(line string) { fmt.Fprintln(os.Stderr, line) })
	if err != nil {
		return err
	}
	fmt.Printf("copied %s into %s (%s) in %s, %d warnings\n", src.Name, dst.Name, *mode, time.Since(start).Round(time.Second), len(report.Warnings))
	for _, w := range report.Warnings {
		fmt.Println("  " + w)
	}
	return nil
}

func dumpCmd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("dump", flag.ExitOnError)
	mode := fs.String("mode", db.CopyAll, "all (schema and data), schema, or data")
	schemas := fs.String("schema", "", "Postgres only: comma-separated schemas to dump (default all)")
	_ = fs.Parse(args)
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return errors.New("usage: slimdb dump [-mode all|schema|data] [-schema a,b] <conn> [file]")
	}
	c, err := st.Conn(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("%q: %w", fs.Arg(0), err)
	}
	opt := db.CopyOptions{Mode: *mode}
	if *schemas != "" {
		opt.Schemas = strings.Split(*schemas, ",")
	}
	if err := db.CheckDump(c, opt); err != nil {
		return err
	}
	path := fs.Arg(1)
	if path == "" {
		path = db.DumpFileName(c, *mode, time.Now())
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	start := time.Now()
	err = db.Dump(ctx, c, opt, f, func(line string) { fmt.Fprintln(os.Stderr, line) })
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return err
	}
	info, _ := os.Stat(path)
	fmt.Printf("dumped %s (%s) into %s, %d bytes in %s\n", c.Name, *mode, path, info.Size(), time.Since(start).Round(time.Second))
	return nil
}

func importCmd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	mode := fs.String("mode", db.CopyAll, "all (replaces the objects in the file), schema, or data (rows into existing tables)")
	schemas := fs.String("schema", "", "Postgres custom-format dumps only: comma-separated schemas to load (default all)")
	yes := fs.Bool("yes", false, "allow a target that is not on this machine")
	_ = fs.Parse(args)
	if fs.NArg() != 2 {
		return errors.New("usage: slimdb import [-mode all|schema|data] [-schema a,b] [-yes] <file> <conn>")
	}
	c, err := st.Conn(fs.Arg(1))
	if err != nil {
		return fmt.Errorf("%q: %w", fs.Arg(1), err)
	}
	if !db.IsLocalTarget(c) && !*yes {
		return fmt.Errorf("%q is not on this machine; pass -yes to overwrite it anyway", c.Name)
	}
	opt := db.CopyOptions{Mode: *mode}
	if *schemas != "" {
		opt.Schemas = strings.Split(*schemas, ",")
	}
	start := time.Now()
	report, err := db.Restore(ctx, c, opt, fs.Arg(0), func(line string) { fmt.Fprintln(os.Stderr, line) })
	if err != nil {
		return err
	}
	fmt.Printf("imported %s into %s (%s) in %s, %d warnings\n", fs.Arg(0), c.Name, *mode, time.Since(start).Round(time.Second), len(report.Warnings))
	for _, w := range report.Warnings {
		fmt.Println("  " + w)
	}
	return nil
}

// displayHost turns a wildcard listen address into one a browser can open.
func displayHost(host string) string {
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		return "localhost"
	}
	return host
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
