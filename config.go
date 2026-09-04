package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Config is everything the UI collects to reach a MySQL server.
type Config struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database"`
	// TLS is one of "false" (default), "preferred", "true", "skip-verify".
	TLS string `json:"tls"`
	// ConnectTimeoutSec bounds the TCP dial + handshake. 0 means 10s.
	ConnectTimeoutSec int `json:"connectTimeoutSec"`
}

func (c Config) addr() string {
	port := c.Port
	if port == 0 {
		port = 3306
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(port))
}

func (c Config) timeout() time.Duration {
	if c.ConnectTimeoutSec <= 0 {
		return 10 * time.Second
	}
	return time.Duration(c.ConnectTimeoutSec) * time.Second
}

// dsn builds a driver DSN. Read/write timeouts are deliberately left unset:
// long scans are bounded by context instead, which cancels cleanly.
func (c Config) dsn() string {
	cfg := mysql.NewConfig()
	cfg.User = c.User
	cfg.Passwd = c.Password
	cfg.Net = "tcp"
	cfg.Addr = c.addr()
	cfg.DBName = c.Database
	cfg.Timeout = c.timeout()
	cfg.AllowNativePasswords = true
	cfg.InterpolateParams = true // one round trip per statement, so timings measure the network, not the prepare dance
	switch c.TLS {
	case "", "false":
		cfg.TLSConfig = "false"
	case "preferred", "true", "skip-verify":
		cfg.TLSConfig = c.TLS
	default:
		cfg.TLSConfig = "false"
	}
	return cfg.FormatDSN()
}

// open returns a pool limited to maxConns, already verified with a ping.
func (c Config) open(ctx context.Context, maxConns int) (*sql.DB, error) {
	db, err := sql.Open("mysql", c.dsn())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(0)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Nanoseconds()) / 1e6
}

// Stats are the summary numbers reported for any set of timing samples (ms).
type Stats struct {
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Avg    float64 `json:"avg"`
	P50    float64 `json:"p50"`
	P95    float64 `json:"p95"`
	P99    float64 `json:"p99"`
	StdDev float64 `json:"stddev"`
	// Jitter is the mean absolute difference between consecutive samples,
	// which is what makes an interactive connection feel bad even at a low average.
	Jitter float64 `json:"jitter"`
}

func summarize(samples []float64) Stats {
	if len(samples) == 0 {
		return Stats{}
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)

	var sum float64
	for _, v := range sorted {
		sum += v
	}
	s := Stats{
		Min: sorted[0],
		Max: sorted[len(sorted)-1],
		Avg: sum / float64(len(sorted)),
		P50: percentile(sorted, 50),
		P95: percentile(sorted, 95),
		P99: percentile(sorted, 99),
	}
	var sq float64
	for _, v := range sorted {
		sq += (v - s.Avg) * (v - s.Avg)
	}
	s.StdDev = math.Sqrt(sq / float64(len(sorted)))

	// Jitter walks the samples in arrival order, not sorted order.
	if len(samples) > 1 {
		var d float64
		for i := 1; i < len(samples); i++ {
			d += math.Abs(samples[i] - samples[i-1])
		}
		s.Jitter = d / float64(len(samples)-1)
	}
	return s
}

// percentile uses the nearest-rank method on an already sorted slice.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return fmt.Sprintf("MySQL error %d: %s", me.Number, me.Message)
	}
	return err.Error()
}

// hint turns a connection failure into the thing to actually go and check.
// Empty string means "no better advice than the error itself".
func hint(err error) string {
	if err == nil {
		return ""
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1045:
			return "Credentials rejected. Check the password, and that the account exists for this client host (MySQL grants are user@host)."
		case 1049:
			return "The server is reachable and the login works, but that database does not exist."
		case 1044:
			return "The login works but the account has no rights on this database. Grant it, or connect without a database."
		case 1130:
			return "The server refuses connections from this client host. The account is likely user@localhost only, or a host ACL blocks you."
		case 1040:
			return "Server is at max_connections. Existing clients are holding connections open."
		case 1226:
			return "The account hit a per-user resource limit (max_user_connections / max_queries_per_hour)."
		case 1698:
			return "The account uses auth_socket and can only log in locally over a unix socket."
		case 2059, 1156:
			return "Auth plugin negotiation failed. MySQL 8 defaults to caching_sha2_password: enable TLS, or switch the account to mysql_native_password."
		}
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no such host"):
		return "DNS could not resolve the hostname. Check spelling, VPN, or /etc/hosts."
	case strings.Contains(msg, "connection refused"):
		return "The host answered but nothing is listening on that port. Check the port number and that mysqld is running."
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "context deadline exceeded"):
		return "No response before the timeout. Usually a firewall or security group silently dropping packets, rather than a slow server."
	case strings.Contains(msg, "network is unreachable"):
		return "No route to the host. Check VPN or routing."
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "x509"):
		return "TLS certificate verification failed. Use TLS mode \"skip-verify\" to confirm that is the only problem."
	case strings.Contains(msg, "malformed packet"), strings.Contains(msg, "unexpected EOF"):
		return "The server closed the connection during handshake. Often TLS required by the server, or a proxy in front of the port that is not MySQL."
	}
	return ""
}
