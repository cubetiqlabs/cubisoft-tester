package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Step is one stage of the connection handshake, timed independently so a slow
// connection can be blamed on the right layer (DNS vs TCP vs auth vs query).
type Step struct {
	Name       string  `json:"name"`
	OK         bool    `json:"ok"`
	Skipped    bool    `json:"skipped"`
	DurationMs float64 `json:"durationMs"`
	Detail     string  `json:"detail"`
	Error      string  `json:"error"`
	Hint       string  `json:"hint"`
}

// ServerInfo is what the server says about itself once we are in.
type ServerInfo struct {
	Version          string   `json:"version"`
	VersionComment   string   `json:"versionComment"`
	Flavor           string   `json:"flavor"` // MySQL, MariaDB, Percona...
	Major            int      `json:"major"`
	Minor            int      `json:"minor"`
	Supported        bool     `json:"supported"` // within the 5.6 - 8.x range this tool targets
	ResolvedAddr     string   `json:"resolvedAddr"`
	ConnectionID     int64    `json:"connectionId"`
	TLSCipher        string   `json:"tlsCipher"`
	CharacterSet     string   `json:"characterSet"`
	Collation        string   `json:"collation"`
	TimeZone         string   `json:"timeZone"`
	SQLMode          string   `json:"sqlMode"`
	MaxConnections   int64    `json:"maxConnections"`
	ThreadsConnected int64    `json:"threadsConnected"`
	MaxAllowedPacket int64    `json:"maxAllowedPacket"`
	WaitTimeoutSec   int64    `json:"waitTimeoutSec"`
	UptimeSec        int64    `json:"uptimeSec"`
	ReadOnly         bool     `json:"readOnly"`
	CurrentUser      string   `json:"currentUser"`
	Grants           []string `json:"grants"`
	ClockSkewMs      float64  `json:"clockSkewMs"`
}

// DiagnoseResult is the whole report for one "Diagnose" run.
type DiagnoseResult struct {
	OK       bool       `json:"ok"`
	Steps    []Step     `json:"steps"`
	Server   ServerInfo `json:"server"`
	Warnings []string   `json:"warnings"`
	TotalMs  float64    `json:"totalMs"`
}

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)`)

// Diagnose walks the connection one layer at a time and stops at the first failure.
// Named results so the deferred TotalMs assignment lands in what the caller gets.
func (t *Tester) Diagnose(cfg Config) (res DiagnoseResult, err error) {
	ctx, done := t.begin(cfg.timeout()*4 + 20*time.Second)
	defer done()

	res = DiagnoseResult{Steps: []Step{}, Warnings: []string{}}
	res.Server.Grants = []string{}
	overall := time.Now()
	defer func() { res.TotalMs = msSince(overall) }()

	add := func(s Step) bool {
		res.Steps = append(res.Steps, s)
		t.progress("diagnose", s.Name, float64(len(res.Steps))/7*100)
		return s.OK
	}
	fail := func(name string, d float64, err error) {
		add(Step{Name: name, DurationMs: d, Error: errString(err), Hint: hint(err)})
	}

	// 1. DNS. Skipped when the host is already an IP, which is worth showing.
	host := cfg.Host
	var ips []string
	start := time.Now()
	if ip := net.ParseIP(host); ip != nil {
		add(Step{Name: "DNS resolve", OK: true, Skipped: true, Detail: "host is already a literal IP"})
		ips = []string{host}
	} else {
		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil {
			fail("DNS resolve", msSince(start), err)
			return res, nil
		}
		ips = addrs
		add(Step{Name: "DNS resolve", OK: true, DurationMs: msSince(start), Detail: strings.Join(addrs, ", ")})
	}

	// 2. Raw TCP. Separating this from the driver connect is the whole point:
	// it splits "network is slow" from "auth is slow".
	start = time.Now()
	conn, err := net.DialTimeout("tcp", cfg.addr(), cfg.timeout())
	if err != nil {
		fail("TCP connect", msSince(start), err)
		return res, nil
	}
	tcpMs := msSince(start)
	res.Server.ResolvedAddr = conn.RemoteAddr().String()
	conn.Close()
	add(Step{Name: "TCP connect", OK: true, DurationMs: tcpMs, Detail: res.Server.ResolvedAddr})

	// 3. Driver connect: handshake, TLS if enabled, and auth.
	start = time.Now()
	db, err := cfg.open(ctx, 1)
	if err != nil {
		fail("MySQL handshake + auth", msSince(start), err)
		return res, nil
	}
	defer db.Close()
	authMs := msSince(start)
	add(Step{
		Name:       "MySQL handshake + auth",
		OK:         true,
		DurationMs: authMs,
		Detail:     fmt.Sprintf("%.1f ms of which %.1f ms was TCP", authMs, tcpMs),
	})

	// 4. Server variables.
	start = time.Now()
	if err := readServerInfo(ctx, db, &res.Server); err != nil {
		fail("Read server variables", msSince(start), err)
		return res, nil
	}
	add(Step{Name: "Read server variables", OK: true, DurationMs: msSince(start),
		Detail: fmt.Sprintf("%s %s", res.Server.Flavor, res.Server.Version)})

	// 5. Grants. Not fatal: plenty of locked-down accounts cannot run SHOW GRANTS.
	start = time.Now()
	grants, err := readGrants(ctx, db)
	if err != nil {
		add(Step{Name: "Privileges", OK: true, Skipped: true, DurationMs: msSince(start),
			Detail: "SHOW GRANTS not permitted: " + errString(err)})
	} else {
		res.Server.Grants = grants
		add(Step{Name: "Privileges", OK: true, DurationMs: msSince(start),
			Detail: fmt.Sprintf("%d grant line(s)", len(grants))})
	}

	// 6. Write probe, so the speed test does not fail later for a boring reason.
	start = time.Now()
	if err := writeProbe(ctx, db); err != nil {
		add(Step{Name: "Write probe (temp table)", DurationMs: msSince(start),
			Error: errString(err), Hint: "Speed tests need CREATE TEMPORARY TABLES plus INSERT on the selected database."})
	} else {
		add(Step{Name: "Write probe (temp table)", OK: true, DurationMs: msSince(start),
			Detail: "create + insert + select + drop"})
	}

	// 7. Round trip on an established connection: the floor for every query.
	start = time.Now()
	var one int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		fail("Query round trip", msSince(start), err)
		return res, nil
	}
	add(Step{Name: "Query round trip", OK: true, DurationMs: msSince(start), Detail: "SELECT 1"})

	res.Warnings = warnings(res.Server, authMs, tcpMs)
	if len(ips) > 1 {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"%s resolves to %d addresses (%s). Each connection may land on a different server, so results can vary between runs.",
			host, len(ips), strings.Join(ips, ", ")))
	}
	res.OK = true
	for _, s := range res.Steps {
		if !s.OK {
			res.OK = false
		}
	}
	return res, nil
}

func readServerInfo(ctx context.Context, db *sql.DB, si *ServerInfo) error {
	// One round trip for everything that lives in a system variable.
	// Every one of these exists in 5.6 through 8.x and in MariaDB.
	const q = `SELECT VERSION(), @@version_comment, @@character_set_server, @@collation_server,
	           @@time_zone, @@sql_mode, @@max_connections, @@max_allowed_packet,
	           @@wait_timeout, @@read_only, CONNECTION_ID(), CURRENT_USER(), UNIX_TIMESTAMP()`
	var serverEpoch float64
	clientEpoch := float64(time.Now().UnixNano()) / 1e9
	err := db.QueryRowContext(ctx, q).Scan(
		&si.Version, &si.VersionComment, &si.CharacterSet, &si.Collation,
		&si.TimeZone, &si.SQLMode, &si.MaxConnections, &si.MaxAllowedPacket,
		&si.WaitTimeoutSec, &si.ReadOnly, &si.ConnectionID, &si.CurrentUser, &serverEpoch)
	if err != nil {
		return err
	}
	si.ClockSkewMs = (serverEpoch - clientEpoch) * 1000

	si.Flavor = "MySQL"
	switch {
	case strings.Contains(si.Version, "MariaDB"):
		si.Flavor = "MariaDB"
	case strings.Contains(strings.ToLower(si.VersionComment), "percona"):
		si.Flavor = "Percona"
	}
	if m := versionRe.FindStringSubmatch(si.Version); m != nil {
		si.Major, _ = strconv.Atoi(m[1])
		si.Minor, _ = strconv.Atoi(m[2])
	}
	si.Supported = si.Flavor != "MariaDB" &&
		((si.Major == 5 && si.Minor >= 6) || si.Major == 8 || si.Major == 9)

	// SHOW STATUS one pattern at a time: information_schema.GLOBAL_STATUS was
	// removed in MySQL 8, and multi-pattern LIKE never existed.
	si.ThreadsConnected = statusInt(ctx, db, "Threads_connected")
	si.UptimeSec = statusInt(ctx, db, "Uptime")
	si.TLSCipher = statusStr(ctx, db, "Ssl_cipher")
	return nil
}

func statusStr(ctx context.Context, db *sql.DB, name string) string {
	var k, v string
	// The variable name is from a fixed internal list, never user input.
	if err := db.QueryRowContext(ctx, "SHOW STATUS LIKE '"+name+"'").Scan(&k, &v); err != nil {
		return ""
	}
	return v
}

func statusInt(ctx context.Context, db *sql.DB, name string) int64 {
	n, _ := strconv.ParseInt(statusStr(ctx, db, name), 10, 64)
	return n
}

func readGrants(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SHOW GRANTS")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// writeProbe proves the account can actually create and write a table without
// leaving anything behind: a TEMPORARY table dies with the connection.
func writeProbe(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "CREATE TEMPORARY TABLE conntest_probe (id INT PRIMARY KEY, v VARCHAR(32)) ENGINE=InnoDB"); err != nil {
		return err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "DROP TEMPORARY TABLE IF EXISTS conntest_probe")
	if _, err := conn.ExecContext(ctx, "INSERT INTO conntest_probe VALUES (1, 'ok')"); err != nil {
		return err
	}
	var v string
	return conn.QueryRowContext(ctx, "SELECT v FROM conntest_probe WHERE id = 1").Scan(&v)
}

// warnings flags the settings that bite later, in production, at 3am.
func warnings(si ServerInfo, authMs, tcpMs float64) []string {
	var w []string
	if !si.Supported {
		w = append(w, fmt.Sprintf("Server is %s %s; this tool targets MySQL 5.6 through 8.x, so some checks may not apply.", si.Flavor, si.Version))
	}
	if si.ReadOnly {
		w = append(w, "Server has read_only=ON. Write and transaction tests will fail unless the account has SUPER.")
	}
	if si.MaxConnections > 0 && float64(si.ThreadsConnected) > 0.8*float64(si.MaxConnections) {
		w = append(w, fmt.Sprintf("Connections are near the limit: %d of %d in use.", si.ThreadsConnected, si.MaxConnections))
	}
	if si.WaitTimeoutSec > 0 && si.WaitTimeoutSec < 60 {
		w = append(w, fmt.Sprintf("wait_timeout is %ds. Pooled connections will be killed while idle; keep the pool lifetime under that.", si.WaitTimeoutSec))
	}
	if si.MaxAllowedPacket > 0 && si.MaxAllowedPacket < 4<<20 {
		w = append(w, fmt.Sprintf("max_allowed_packet is only %s. Large batches and BLOBs will be rejected.", humanBytes(si.MaxAllowedPacket)))
	}
	if si.TLSCipher == "" {
		w = append(w, "This connection is not encrypted. Traffic, including the query text, crosses the network in the clear.")
	}
	if !strings.Contains(si.SQLMode, "STRICT") {
		w = append(w, "sql_mode has no STRICT setting. Bad values are silently truncated rather than rejected.")
	}
	if si.Major == 5 && si.Minor == 6 {
		w = append(w, "MySQL 5.6 reached end of life in February 2021 and receives no security fixes.")
	}
	if si.ClockSkewMs > 5000 || si.ClockSkewMs < -5000 {
		w = append(w, fmt.Sprintf("Server clock differs from this machine by %.0f s. Time-based queries and TLS validity checks may misbehave.", si.ClockSkewMs/1000))
	}
	if authMs-tcpMs > 250 {
		w = append(w, fmt.Sprintf("Auth added %.0f ms on top of the TCP connect. Check skip_name_resolve: the server may be doing a reverse DNS lookup on every connection.", authMs-tcpMs))
	}
	if w == nil {
		return []string{}
	}
	return w
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
