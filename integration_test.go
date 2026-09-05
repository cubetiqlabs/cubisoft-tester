package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testConfig reads a live server from the environment. Set MYSQLTEST_HOST to
// run these; without it they skip, so `go test ./...` stays offline-friendly.
//
//	docker run -d --rm -e MYSQL_ROOT_PASSWORD=testpw -e MYSQL_DATABASE=testdb \
//	  -p 13306:3306 mysql:8.0
//	MYSQLTEST_HOST=127.0.0.1 MYSQLTEST_PORT=13306 go test -run Live ./...
func testConfig(t *testing.T) Config {
	t.Helper()
	host := os.Getenv("MYSQLTEST_HOST")
	if host == "" {
		t.Skip("set MYSQLTEST_HOST to run live MySQL tests")
	}
	port, _ := strconv.Atoi(os.Getenv("MYSQLTEST_PORT"))
	if port == 0 {
		port = 3306
	}
	return Config{
		Host:     host,
		Port:     port,
		User:     envOr("MYSQLTEST_USER", "root"),
		Password: envOr("MYSQLTEST_PASSWORD", "testpw"),
		Database: envOr("MYSQLTEST_DB", "testdb"),
	}
}

func envOr(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func TestLiveDiagnose(t *testing.T) {
	cfg := testConfig(t)
	res, err := (&Tester{}).Diagnose(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Steps {
		t.Logf("%-28s ok=%v %7.2fms %s%s", s.Name, s.OK, s.DurationMs, s.Detail, s.Error)
	}
	if !res.OK {
		t.Fatal("diagnose failed")
	}
	if res.Server.Version == "" || res.Server.Major == 0 {
		t.Fatalf("version not parsed: %+v", res.Server)
	}
	if !res.Server.Supported {
		t.Fatalf("server %s reported unsupported", res.Server.Version)
	}
	if res.Server.ConnectionID == 0 {
		t.Fatal("no connection id")
	}
	t.Logf("server: %s %s, warnings: %v", res.Server.Flavor, res.Server.Version, res.Warnings)
}

func TestLiveListDatabases(t *testing.T) {
	cfg := testConfig(t)
	dbs, err := (&Tester{}).ListDatabases(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(dbs, "mysql") {
		t.Fatalf("expected the mysql schema in %v", dbs)
	}
}

func TestLiveLatency(t *testing.T) {
	cfg := testConfig(t)
	res, err := (&Tester{}).Latency(LatencyOptions{Config: cfg, Count: 5, IntervalMs: 1, ColdConnects: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Series) != 3 {
		t.Fatalf("want 3 series, got %d", len(res.Series))
	}
	for _, s := range res.Series {
		t.Logf("%-24s sent=%d lost=%d p50=%.2fms p99=%.2fms jitter=%.2f", s.Name, s.Sent, s.Lost, s.Stats.P50, s.Stats.P99, s.Stats.Jitter)
		if s.Lost > 0 {
			t.Errorf("%s lost %d probes: %s", s.Name, s.Lost, s.Error)
		}
		if s.Stats.P50 <= 0 {
			t.Errorf("%s has no timing data", s.Name)
		}
	}
	t.Log("verdict:", res.Verdict)
}

func TestLiveSpeedTest(t *testing.T) {
	cfg := testConfig(t)
	res, err := (&Tester{}).SpeedTest(SpeedOptions{
		Config: cfg, Rows: 500, PayloadBytes: 512, BatchSize: 100, Transactions: 20, Concurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res.Phases {
		t.Logf("%-52s ok=%v %8.1fms rows=%d %.2fMiB/s %s%s", p.Name, p.OK, p.DurationMs, p.Rows, p.MiBPerSec, p.Detail, p.Error)
	}
	if !res.OK {
		t.Fatalf("speed test failed; warnings: %v", res.Warnings)
	}
	// The insert phase must have written exactly what we asked for.
	for _, p := range res.Phases {
		if strings.HasPrefix(p.Name, "Insert") && p.Rows != 500 {
			t.Fatalf("inserted %d rows, want 500", p.Rows)
		}
		// Delete removes the bulk rows plus the ones the transaction phase committed.
		if p.Name == "Delete: empty the table" && p.Rows != 520 {
			t.Fatalf("deleted %d rows, want 520", p.Rows)
		}
	}
	t.Log("summary:", res.Summary)

	// The table must be gone afterwards; a diagnostics tool that litters is worse than none.
	db, err := cfg.open(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?", res.Table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("test table %s was left behind", res.Table)
	}
}

func TestLiveTrace(t *testing.T) {
	cfg := testConfig(t)
	res, err := (&Tester{}).Trace(TraceOptions{Config: cfg, MaxHops: 5, Probes: 2, TimeoutMs: 800, ResolveNames: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("mode=%s target=%s reached=%v portOpen=%v (%.2fms) note=%s",
		res.Mode, res.TargetIP, res.Reached, res.PortOpen, res.PortMs, res.Note)
	for _, h := range res.Hops {
		t.Logf("  %2d  %-16s %-30s %v", h.TTL, h.Addr, h.Host, h.RTTs)
	}
	if !res.PortOpen {
		t.Fatalf("TCP probe to the MySQL port failed: %s", res.PortError)
	}
}

// TestLiveBadCredentials checks the diagnosis path, which is the half of this
// tool that only ever runs when something is already wrong.
func TestLiveBadCredentials(t *testing.T) {
	cfg := testConfig(t)
	cfg.Password = "definitely-not-the-password"
	res, err := (&Tester{}).Diagnose(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("expected failure with a bad password")
	}
	last := res.Steps[len(res.Steps)-1]
	if last.Name != "MySQL handshake + auth" || last.OK {
		t.Fatalf("expected auth to be the failing step, got %+v", last)
	}
	if !strings.Contains(last.Hint, "password") {
		t.Fatalf("no useful hint: %q", last.Hint)
	}
	// TCP must still have succeeded: that separation is the point of the report.
	if !res.Steps[1].OK {
		t.Fatal("TCP step should still pass when only the password is wrong")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestLiveListTables(t *testing.T) {
	cfg := testConfig(t)
	tester := &Tester{}
	seedTable(t, cfg, "conntest_inventory")

	tables, err := tester.ListTables(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var found *TableInfo
	for i := range tables {
		if tables[i].Name == "conntest_inventory" {
			found = &tables[i]
		}
	}
	if found == nil {
		t.Fatalf("seeded table missing from %d listed tables", len(tables))
	}
	if !found.HasPK {
		t.Error("primary key not detected")
	}
	if !strings.EqualFold(found.Engine, "InnoDB") {
		t.Errorf("engine = %s", found.Engine)
	}
	t.Logf("%s: engine=%s rows=%d data=%d", found.Name, found.Engine, found.Rows, found.DataBytes)
}

// TestLiveBackupRestore is the round trip that matters: dump, destroy, restore,
// and check the rows came back.
func TestLiveBackupRestore(t *testing.T) {
	cfg := testConfig(t)
	tester := &Tester{}
	if box := tester.Toolbox(); !box.Dump.Found || !box.Client.Found {
		t.Skipf("mysqldump/mysql not installed: %s", box.Hint)
	}
	seedTable(t, cfg, "conntest_backup")
	dump := filepath.Join(t.TempDir(), "dump.sql.gz")

	res, err := tester.Backup(BackupOptions{
		Config: cfg, Tables: []string{"conntest_backup"},
		AddDropTable: true, SingleTransaction: true, Compress: true, Path: dump,
	})
	if err != nil {
		t.Fatalf("backup: %v (stderr %s)", err, res.Stderr)
	}
	if res.Bytes == 0 {
		t.Fatal("dump is empty")
	}
	// The password must never reach the reported command line.
	if strings.Contains(res.Command, cfg.Password) {
		t.Fatalf("password leaked into the command: %s", res.Command)
	}
	t.Logf("dumped %d bytes in %.0fms", res.Bytes, res.DurationMs)

	mustExec(t, cfg, "DROP TABLE conntest_backup")

	rr, err := tester.Restore(RestoreOptions{Config: cfg, Confirm: true, Path: dump})
	if err != nil {
		t.Fatalf("restore: %v (stderr %s)", err, rr.Stderr)
	}
	if n := countRows(t, cfg, "conntest_backup"); n != 3 {
		t.Fatalf("restored %d rows, want 3", n)
	}
	mustExec(t, cfg, "DROP TABLE IF EXISTS conntest_backup")

	// Restoring without the confirmation flag must be refused outright.
	if _, err := tester.Restore(RestoreOptions{Config: cfg, Path: dump}); err == nil {
		t.Fatal("restore ran without confirmation")
	}
}

func seedTable(t *testing.T, cfg Config, name string) {
	t.Helper()
	mustExec(t, cfg, "DROP TABLE IF EXISTS "+name)
	mustExec(t, cfg, "CREATE TABLE "+name+" (id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, label VARCHAR(64) NOT NULL) ENGINE=InnoDB")
	mustExec(t, cfg, "INSERT INTO "+name+" (label) VALUES ('one'),('two'),('three')")
	t.Cleanup(func() { mustExec(t, cfg, "DROP TABLE IF EXISTS "+name) })
}

// Deliberately not t.Context(): it is cancelled before t.Cleanup runs, and
// these helpers are used from cleanup to drop the seeded tables.
func mustExec(t *testing.T, cfg Config, query string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := cfg.open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, query); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func countRows(t *testing.T, cfg Config, table string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := cfg.open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
