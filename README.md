# Cubisoft - MySQL Tester

A desktop app (Wails v3 + Go) for diagnosing a MySQL connection from the machine
that actually has the problem — the client. Targets MySQL 5.6 through 8.x.

Four tools, all against a real connection:

| Tab | What it answers |
|---|---|
| **Diagnose** | Where exactly does the connection break, and what is this server configured like? |
| **Latency** | How long does a round trip take, how much does it vary, and what does a fresh connection cost? |
| **Trace route** | What network path do packets take, and is the MySQL port itself reachable? |
| **Speed test** | How fast can this client actually insert, read, update, commit and delete? |

## Running it

```sh
go install github.com/wailsapp/wails/v3/cmd/wails3@latest
wails3 task dev      # live reload
wails3 task build    # binary in bin/
wails3 task package  # .app / .exe / AppImage
```

## What each tab does

**Diagnose** times DNS, TCP, the MySQL handshake, auth, a privilege check, a write
probe and a query round trip *separately*, and stops at the first failure. That
split is the point: "the database is slow" usually means one specific layer is
slow, and this says which. Failures come with a hint — access denied points at
`user@host` grants, a timeout points at a firewall rather than a busy server.
It then reads the server's settings and flags the ones that bite later:
`read_only`, a short `wait_timeout`, a small `max_allowed_packet`, a missing
`STRICT` mode, an unencrypted connection, clock skew, connections near the cap,
and reverse-DNS-on-connect (`skip_name_resolve`).

**Latency** runs three series — bare TCP handshake, `SELECT 1` on an open
connection, and a full cold connect — and reports min/median/mean/p95/p99/max,
jitter and loss for each, with a bar per probe in arrival order. The gap between
the three is what tells you how to size a pool.

**Trace route** is a TTL-limited ICMP traceroute plus a TCP connect to the MySQL
port. It runs unprivileged on macOS and on Linux where `ping_group_range` allows
it, and falls back to raw ICMP (root) otherwise; if neither works it says so and
the TCP check still stands. Servers that drop ICMP show as `*` — the TCP result
is the one that answers "can I reach the database".

**Speed test** creates a throwaway `conntest_*` table in the selected database and
runs real work through it: batched multi-row inserts (optionally across several
connections), a full table scan, point lookups by primary key, a bulk update,
timed commit cycles, a rollback correctness check, and a delete. It drops the
table afterwards, including when the run is cancelled. Batch size is capped to fit
the server's `max_allowed_packet`.

## Tests

```sh
go test ./                                   # unit tests, no server needed

docker run -d --rm -e MYSQL_ROOT_PASSWORD=testpw -e MYSQL_DATABASE=testdb \
  -p 13306:3306 mysql:8.0
MYSQLTEST_HOST=127.0.0.1 MYSQLTEST_PORT=13306 go test -run Live -v ./
```

The live tests cover diagnose, latency, traceroute, the full speed test (including
that it leaves no table behind) and the bad-password diagnosis path. They have been
run against MySQL 8.0 and 5.7.

## Notes

- The password is never written to disk; the rest of the connection form is kept
  in `localStorage` between runs.
- `go build` alone needs `frontend/dist` to exist (it is embedded). The Wails
  tasks build the frontend first; a `.gitkeep` keeps a bare `go test` working.

# Contributors

- Sambo Chea <sombochea@cubis.tech>
