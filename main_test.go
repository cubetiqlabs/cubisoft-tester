package main

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestSummarize(t *testing.T) {
	s := summarize([]float64{10, 20, 30, 40, 100})
	if !near(s.Min, 10) || !near(s.Max, 100) || !near(s.Avg, 40) {
		t.Fatalf("min/max/avg wrong: %+v", s)
	}
	if !near(s.P50, 30) || !near(s.P95, 100) || !near(s.P99, 100) {
		t.Fatalf("percentiles wrong: %+v", s)
	}
	// Jitter must follow arrival order, not sorted order: |10|+|10|+|10|+|60| / 4.
	if !near(s.Jitter, 22.5) {
		t.Fatalf("jitter = %v, want 22.5", s.Jitter)
	}
	if got := summarize(nil); got != (Stats{}) {
		t.Fatalf("empty input should give zero Stats, got %+v", got)
	}
	if one := summarize([]float64{7}); !near(one.P99, 7) || !near(one.Jitter, 0) {
		t.Fatalf("single sample: %+v", one)
	}
}

func TestOkSamplesDropsTimeouts(t *testing.T) {
	got := okSamples([]float64{5, -1, 7})
	if len(got) != 2 || !near(got[0], 5) || !near(got[1], 7) {
		t.Fatalf("got %v", got)
	}
}

func TestDSN(t *testing.T) {
	c := Config{Host: "db.example.com", Port: 3307, User: "u", Password: "p@ss", Database: "app", TLS: "skip-verify"}
	dsn := c.dsn()
	for _, want := range []string{"u:p@ss@tcp(db.example.com:3307)/app", "tls=skip-verify", "interpolateParams=true", "timeout=10s"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("dsn %q missing %q", dsn, want)
		}
	}
	if _, err := mysql.ParseDSN(dsn); err != nil {
		t.Fatalf("driver rejects our own DSN: %v", err)
	}
	// An IPv6 literal host must still produce a parseable address.
	v6 := Config{Host: "::1", User: "u"}.dsn()
	if !strings.Contains(v6, "tcp([::1]:3306)") {
		t.Fatalf("ipv6 host not bracketed: %s", v6)
	}
	// An unknown TLS value must fail closed to plaintext rather than to a bad DSN.
	if bad := (Config{Host: "h", User: "u", TLS: "nonsense"}).dsn(); !strings.Contains(bad, "tls=false") {
		t.Fatalf("unknown tls mode not normalised: %s", bad)
	}
}

func TestHint(t *testing.T) {
	if hint(nil) != "" {
		t.Fatal("nil error should have no hint")
	}
	if h := hint(&mysql.MySQLError{Number: 1045}); !strings.Contains(h, "password") {
		t.Fatalf("1045 hint = %q", h)
	}
	if h := hint(errors.New("dial tcp 1.2.3.4:3306: connect: connection refused")); !strings.Contains(h, "nothing is listening") {
		t.Fatalf("refused hint = %q", h)
	}
	if h := hint(errors.New("something nobody has ever seen")); h != "" {
		t.Fatalf("unknown error should have no hint, got %q", h)
	}
}

// embeddedSeq is the one piece of traceroute that silently breaks everything if
// it is wrong: get it wrong and every hop reads as a timeout.
func TestEmbeddedSeq(t *testing.T) {
	original, err := (&icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Body: &icmp.Echo{ID: 1234, Seq: 4242, Data: []byte("wails-mysql-trace")},
	}).Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	// A router quotes the IPv4 header of the packet it dropped, then its payload.
	iphdr := []byte{0x45, 0, 0, 0x3c, 0, 0, 0, 0, 1, 1, 0, 0, 10, 0, 0, 1, 10, 0, 0, 2}
	if got := embeddedSeq(append(iphdr, original...)); got != 4242 {
		t.Fatalf("embeddedSeq = %d, want 4242", got)
	}
	if got := embeddedSeq([]byte{1, 2, 3}); got != -1 {
		t.Fatalf("truncated packet should give -1, got %d", got)
	}
}

func TestBiggestJump(t *testing.T) {
	hops := []Hop{{TTL: 1, AvgMs: 1}, {TTL: 2, AvgMs: 2}, {TTL: 3, AvgMs: -1}, {TTL: 4, AvgMs: 90}, {TTL: 5, AvgMs: 92}}
	if got := biggestJump(hops); got != 4 {
		t.Fatalf("biggestJump = %d, want 4 (the ocean crossing)", got)
	}
	if got := biggestJump(nil); got != 0 {
		t.Fatalf("no hops should give 0, got %d", got)
	}
}

func TestHumanBytes(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{{512, "512 B"}, {1024, "1.0 KiB"}, {4 << 20, "4.0 MiB"}, {1 << 30, "1.0 GiB"}} {
		if got := humanBytes(c.in); got != c.want {
			t.Fatalf("humanBytes(%d) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestRandPayloadIsEscapeFree(t *testing.T) {
	p := randPayload(4096)
	if len(p) != 4096 {
		t.Fatalf("length = %d", len(p))
	}
	// Quotes or backslashes would inflate the interpolated statement and skew
	// every throughput number the speed test reports.
	if strings.ContainsAny(p, "'\"\\\x00\n\r") {
		t.Fatal("payload contains characters the driver would escape")
	}
}

func TestRatePhase(t *testing.T) {
	p := Phase{Rows: 1000, Bytes: 10 << 20, DurationMs: 2000}
	rate(&p)
	if !near(p.RowsPerSec, 500) || !near(p.MiBPerSec, 5) {
		t.Fatalf("rate = %+v", p)
	}
	zero := Phase{Rows: 5}
	rate(&zero)
	if zero.RowsPerSec != 0 {
		t.Fatal("zero duration must not divide by zero")
	}
}

func TestWarningsFlagsTheDangerousDefaults(t *testing.T) {
	si := ServerInfo{Flavor: "MySQL", Version: "5.6.51", Major: 5, Minor: 6, Supported: true,
		ReadOnly: true, MaxConnections: 100, ThreadsConnected: 95, WaitTimeoutSec: 30,
		MaxAllowedPacket: 1 << 20, SQLMode: "NO_ENGINE_SUBSTITUTION"}
	got := strings.Join(warnings(si, 500, 10), "\n")
	for _, want := range []string{"read_only", "near the limit", "wait_timeout", "max_allowed_packet", "not encrypted", "STRICT", "end of life", "skip_name_resolve"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing warning about %q in:\n%s", want, got)
		}
	}
	healthy := ServerInfo{Flavor: "MySQL", Version: "8.0.36", Major: 8, Minor: 0, Supported: true,
		MaxConnections: 500, ThreadsConnected: 10, WaitTimeoutSec: 28800,
		MaxAllowedPacket: 64 << 20, SQLMode: "STRICT_TRANS_TABLES", TLSCipher: "TLS_AES_256_GCM_SHA384"}
	if w := warnings(healthy, 20, 15); len(w) != 0 {
		t.Fatalf("healthy server should produce no warnings, got %v", w)
	}
}
