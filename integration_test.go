package main

import (
	"os"
	"strconv"
	"strings"
	"testing"
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
