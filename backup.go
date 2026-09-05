package main

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
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Tool is an external MySQL client binary we shell out to. Writing a correct
// dumper (every type, trigger, routine, charset, view ordering) is a project of
// its own; mysqldump already is that project.
type Tool struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version"`
	Found   bool   `json:"found"`
}

// Toolbox reports which external binaries are usable on this machine.
type Toolbox struct {
	Dump   Tool   `json:"dump"`
	Client Tool   `json:"client"`
	Hint   string `json:"hint"`
}

// extraToolDirs are the places these binaries hide when they are installed but
// not on PATH, which is the normal case for GUI apps launched from Finder.
func extraToolDirs() []string {
	switch runtime.GOOS {
	case "darwin":
		// mysql-client is keg-only in Homebrew, so it never lands on PATH.
		return []string{
			"/opt/homebrew/bin", "/usr/local/bin", "/usr/local/mysql/bin", "/opt/local/bin",
			"/opt/homebrew/opt/mysql-client/bin", "/opt/homebrew/opt/mysql/bin",
			"/usr/local/opt/mysql-client/bin", "/usr/local/opt/mysql/bin",
		}
	case "windows":
		return []string{
			`C:\Program Files\MySQL\MySQL Server 8.0\bin`,
			`C:\Program Files\MySQL\MySQL Server 8.4\bin`,
			`C:\Program Files\MariaDB 11.4\bin`,
			`C:\Program Files\MySQL\MySQL Workbench 8.0 CE`,
		}
	default:
		return []string{"/usr/bin", "/usr/local/bin", "/opt/mysql/bin"}
	}
}

func findTool(names ...string) Tool {
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	for _, name := range names {
		if p, err := exec.LookPath(name + exe); err == nil {
			return describeTool(name, p)
		}
		for _, dir := range extraToolDirs() {
			p := filepath.Join(dir, name+exe)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return describeTool(name, p)
			}
		}
	}
	return Tool{Name: names[0]}
}

func describeTool(name, path string) Tool {
	t := Tool{Name: name, Path: path, Found: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, path, "--version").Output(); err == nil {
		t.Version = strings.TrimSpace(string(out))
	}
	return t
}

// Toolbox tells the UI up front whether backup and restore are available.
func (t *Tester) Toolbox() Toolbox {
	box := Toolbox{
		Dump:   findTool("mysqldump", "mariadb-dump"),
		Client: findTool("mysql", "mariadb"),
	}
	if !box.Dump.Found || !box.Client.Found {
		switch runtime.GOOS {
		case "darwin":
			box.Hint = "Install the client tools with: brew install mysql-client"
		case "windows":
			box.Hint = "Install MySQL Server or the MySQL Shell, then make sure its bin directory is on PATH."
		default:
			box.Hint = "Install the client tools with: apt install mysql-client (or mariadb-client)"
		}
	}
	return box
}

// TableInfo is one row of the database overview shown before a backup.
type TableInfo struct {
	Name       string `json:"name"`
	Engine     string `json:"engine"`
	Rows       int64  `json:"rows"`
	DataBytes  int64  `json:"dataBytes"`
	IndexBytes int64  `json:"indexBytes"`
	Collation  string `json:"collation"`
	HasPK      bool   `json:"hasPk"`
}

// ListTables shows what is in the database and how big it is, largest first —
// both to size a backup and to spot the tables that will hurt.
func (t *Tester) ListTables(cfg Config) ([]TableInfo, error) {
	if cfg.Database == "" {
		return nil, errors.New("select a database first")
	}
	ctx, done := t.begin(cfg.timeout() + 30*time.Second)
	defer done()

	db, err := cfg.open(ctx, 1)
	if err != nil {
		return nil, errorWithHint(err)
	}
	defer db.Close()

	// information_schema.STATISTICS is the portable way to spot a missing
	// primary key across 5.6 through 8.x.
	withPK := map[string]bool{}
	if rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT TABLE_NAME FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE() AND INDEX_NAME = 'PRIMARY'`); err == nil {
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil {
				withPK[name] = true
			}
		}
		rows.Close()
	}

	rows, err := db.QueryContext(ctx,
		`SELECT TABLE_NAME, IFNULL(ENGINE,''), IFNULL(TABLE_ROWS,0),
		        IFNULL(DATA_LENGTH,0), IFNULL(INDEX_LENGTH,0), IFNULL(TABLE_COLLATION,'')
		 FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE'
		 ORDER BY (IFNULL(DATA_LENGTH,0) + IFNULL(INDEX_LENGTH,0)) DESC`)
	if err != nil {
		return nil, errorWithHint(err)
	}
	defer rows.Close()

	out := []TableInfo{}
	for rows.Next() {
		var ti TableInfo
		if err := rows.Scan(&ti.Name, &ti.Engine, &ti.Rows, &ti.DataBytes, &ti.IndexBytes, &ti.Collation); err != nil {
			return nil, err
		}
		ti.HasPK = withPK[ti.Name]
		out = append(out, ti)
	}
	return out, rows.Err()
}

/* ---------- backup ---------- */

// BackupOptions mirrors the mysqldump switches worth exposing.
type BackupOptions struct {
	Config            Config   `json:"config"`
	Tables            []string `json:"tables"` // empty means the whole database
	SchemaOnly        bool     `json:"schemaOnly"`
	DataOnly          bool     `json:"dataOnly"`
	AddDropTable      bool     `json:"addDropTable"`
	Routines          bool     `json:"routines"`
	Triggers          bool     `json:"triggers"`
	Events            bool     `json:"events"`
	SingleTransaction bool     `json:"singleTransaction"`
	Compress          bool     `json:"compress"`
	// Path writes straight to this file. Blank opens a save dialog, which is
	// what the UI does; tests pass a path.
	Path string `json:"path"`
}

// BackupResult is what the UI reports after a dump.
type BackupResult struct {
	Path       string  `json:"path"`
	Bytes      int64   `json:"bytes"`
	DurationMs float64 `json:"durationMs"`
	Command    string  `json:"command"`
	Stderr     string  `json:"stderr"`
}

// defaultsFile writes credentials to a 0600 temp file. Passing them as
// command-line flags would expose the password to every process listing on the
// machine, which mysqldump itself warns about.
func defaultsFile(cfg Config) (string, error) {
	f, err := os.CreateTemp("", "mysqltester-*.cnf")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	var b strings.Builder
	b.WriteString("[client]\n")
	fmt.Fprintf(&b, "host=%s\n", cfg.Host)
	port := cfg.Port
	if port == 0 {
		port = 3306
	}
	fmt.Fprintf(&b, "port=%d\n", port)
	fmt.Fprintf(&b, "user=%s\n", cfg.User)
	if cfg.Password != "" {
		fmt.Fprintf(&b, "password=%s\n", cfg.Password)
	}
	switch cfg.TLS {
	case "true":
		b.WriteString("ssl-mode=VERIFY_IDENTITY\n")
	case "skip-verify", "preferred":
		b.WriteString("ssl-mode=REQUIRED\n")
	}
	if _, err := f.WriteString(b.String()); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func (o BackupOptions) args(defaults string) []string {
	// --defaults-file must come first or the client ignores it.
	args := []string{"--defaults-file=" + defaults, "--quick", "--default-character-set=utf8mb4"}
	if o.SingleTransaction {
		args = append(args, "--single-transaction")
	}
	if o.SchemaOnly {
		args = append(args, "--no-data")
	}
	if o.DataOnly {
		args = append(args, "--no-create-info")
	}
	if o.AddDropTable {
		args = append(args, "--add-drop-table")
	} else {
		args = append(args, "--skip-add-drop-table")
	}
	if o.Routines {
		args = append(args, "--routines")
	}
	if o.Triggers {
		args = append(args, "--triggers")
	} else {
		args = append(args, "--skip-triggers")
	}
	if o.Events {
		args = append(args, "--events")
	}
	args = append(args, o.Config.Database)
	return append(args, o.Tables...)
}

// Backup shells out to mysqldump and streams its output to a file the user picks.
func (t *Tester) Backup(opts BackupOptions) (BackupResult, error) {
	var res BackupResult
	if opts.Config.Database == "" {
		return res, errors.New("select a database first")
	}
	if opts.SchemaOnly && opts.DataOnly {
		return res, errors.New("schema-only and data-only cannot both be set")
	}
	tool := findTool("mysqldump", "mariadb-dump")
	if !tool.Found {
		return res, errors.New("mysqldump was not found on this machine; see the note above the button")
	}

	path := opts.Path
	if path == "" {
		app := application.Get()
		if app == nil {
			return res, errors.New("no application window")
		}
		name := fmt.Sprintf("%s-%s.sql", opts.Config.Database, time.Now().Format("20060102-150405"))
		if opts.Compress {
			name += ".gz"
		}
		var err error
		path, err = app.Dialog.SaveFile().
			SetMessage("Save backup").
			SetFilename(name).
			AddFilter("SQL dump", "*.sql;*.gz").
			PromptForSingleSelection()
		if err != nil || path == "" {
			return res, err
		}
	}

	ctx, done := t.begin(6 * time.Hour)
	defer done()

	defaults, err := defaultsFile(opts.Config)
	if err != nil {
		return res, err
	}
	defer os.Remove(defaults)

	args := opts.args(defaults)
	res.Command = tool.Path + " " + strings.Join(append([]string{"--defaults-file=<temp>"}, args[1:]...), " ")

	out, err := os.Create(path)
	if err != nil {
		return res, err
	}
	defer out.Close()

	var sink io.Writer = out
	var gz *gzip.Writer
	if opts.Compress {
		gz = gzip.NewWriter(out)
		sink = gz
	}
	counter := &countingWriter{w: bufio.NewWriterSize(sink, 256<<10)}

	cmd := exec.CommandContext(ctx, tool.Path, args...)
	cmd.Stdout = counter
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	start := time.Now()
	stop := t.reportBytes("backup", counter)
	err = cmd.Run()
	stop()
	counter.w.Flush()
	if gz != nil {
		gz.Close()
	}

	res.DurationMs = msSince(start)
	res.Bytes = counter.n.Load()
	res.Stderr = strings.TrimSpace(stderr.String())
	res.Path = path
	if err != nil {
		os.Remove(path) // a half-written dump is worse than none: it looks restorable
		msg := res.Stderr
		if msg == "" {
			msg = err.Error()
		}
		return res, fmt.Errorf("mysqldump failed: %s", msg)
	}
	return res, nil
}

/* ---------- restore ---------- */

// RestoreOptions describes one restore run. Confirm must be set by the UI.
type RestoreOptions struct {
	Config  Config `json:"config"`
	Confirm bool   `json:"confirm"`
	// Path reads straight from this file. Blank opens a file dialog.
	Path string `json:"path"`
}

// RestoreResult is what the UI reports after a restore.
type RestoreResult struct {
	Path       string  `json:"path"`
	Bytes      int64   `json:"bytes"`
	DurationMs float64 `json:"durationMs"`
	Stderr     string  `json:"stderr"`
}

// Restore pipes a dump file into the mysql client. It overwrites whatever the
// dump touches, so the UI has to pass Confirm.
func (t *Tester) Restore(opts RestoreOptions) (RestoreResult, error) {
	var res RestoreResult
	if opts.Config.Database == "" {
		return res, errors.New("select the database to restore into")
	}
	if !opts.Confirm {
		return res, errors.New("restore not confirmed")
	}
	tool := findTool("mysql", "mariadb")
	if !tool.Found {
		return res, errors.New("the mysql client was not found on this machine; see the note above the button")
	}

	path := opts.Path
	if path == "" {
		app := application.Get()
		if app == nil {
			return res, errors.New("no application window")
		}
		var err error
		path, err = app.Dialog.OpenFile().
			SetTitle("Restore from dump").
			SetMessage("Choose a .sql or .sql.gz dump").
			AddFilter("SQL dump", "*.sql;*.gz").
			CanChooseFiles(true).
			PromptForSingleSelection()
		if err != nil || path == "" {
			return res, err
		}
	}
	res.Path = path

	ctx, done := t.begin(6 * time.Hour)
	defer done()

	defaults, err := defaultsFile(opts.Config)
	if err != nil {
		return res, err
	}
	defer os.Remove(defaults)

	f, err := os.Open(path)
	if err != nil {
		return res, err
	}
	defer f.Close()

	var src io.Reader = f
	if strings.HasSuffix(strings.ToLower(path), ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return res, fmt.Errorf("not a gzip file: %w", err)
		}
		defer gz.Close()
		src = gz
	}
	counter := &countingReader{r: src}

	cmd := exec.CommandContext(ctx, tool.Path,
		"--defaults-file="+defaults, "--default-character-set=utf8mb4", opts.Config.Database)
	cmd.Stdin = counter
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	start := time.Now()
	stop := t.reportBytes("restore", counter)
	err = cmd.Run()
	stop()

	res.DurationMs = msSince(start)
	res.Bytes = counter.n.Load()
	res.Stderr = strings.TrimSpace(stderr.String())
	if err != nil {
		msg := res.Stderr
		if msg == "" {
			msg = err.Error()
		}
		return res, fmt.Errorf("restore failed: %s", msg)
	}
	return res, nil
}

/* ---------- byte counting ---------- */

type byteCounter interface{ count() int64 }

type countingWriter struct {
	w *bufio.Writer
	n atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *countingWriter) count() int64 { return c.n.Load() }

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *countingReader) count() int64 { return c.n.Load() }

// reportBytes ticks progress while a dump or restore streams. There is no total
// to compare against, so it reports throughput rather than a percentage.
func (t *Tester) reportBytes(kind string, c byteCounter) func() {
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(400 * time.Millisecond)
		defer tick.Stop()
		start := time.Now()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				n := c.count()
				rate := float64(n) / (1 << 20) / time.Since(start).Seconds()
				t.progress(kind, fmt.Sprintf("%s transferred (%.1f MiB/s)", humanBytes(n), rate), -1)
			}
		}
	}()
	return func() { close(done) }
}
