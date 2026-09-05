package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SpeedOptions controls one throughput run.
type SpeedOptions struct {
	Config Config `json:"config"`
	// Rows to insert in the bulk phase. 0 means 5000.
	Rows int `json:"rows"`
	// PayloadBytes per row. 0 means 256.
	PayloadBytes int `json:"payloadBytes"`
	// BatchSize is rows per INSERT statement. 0 means 200. Capped to fit max_allowed_packet.
	BatchSize int `json:"batchSize"`
	// Transactions is how many single-row commit cycles to time. 0 means 100.
	Transactions int `json:"transactions"`
	// Concurrency is the number of parallel connections. 0 means 1.
	Concurrency int `json:"concurrency"`
	// KeepTable leaves the test table behind for inspection.
	KeepTable bool `json:"keepTable"`
}

// Phase is one timed stage of the speed test.
type Phase struct {
	Name string `json:"name"`
	// Key is the short label the live chart groups by, so a phase's streaming
	// samples and its closing sample land on the same line.
	Key        string  `json:"key"`
	OK         bool    `json:"ok"`
	Rows       int64   `json:"rows"`
	Bytes      int64   `json:"bytes"`
	DurationMs float64 `json:"durationMs"`
	RowsPerSec float64 `json:"rowsPerSec"`
	MiBPerSec  float64 `json:"miBPerSec"`
	OpsPerSec  float64 `json:"opsPerSec"`
	Stats      Stats   `json:"stats"` // per-operation latency, where the phase has discrete ops
	Detail     string  `json:"detail"`
	Error      string  `json:"error"`
	Hint       string  `json:"hint"`
}

// SpeedResult is the whole throughput report.
type SpeedResult struct {
	OK       bool     `json:"ok"`
	Table    string   `json:"table"`
	Phases   []Phase  `json:"phases"`
	Summary  string   `json:"summary"`
	Warnings []string `json:"warnings"`
	TotalMs  float64  `json:"totalMs"`
}

// Sample is one live throughput reading, emitted on "speed:sample" while a
// speed test runs so the UI can draw the run as it happens.
type Sample struct {
	Phase      string  `json:"phase"`
	TMs        float64 `json:"tMs"`
	RowsPerSec float64 `json:"rowsPerSec"`
	MiBPerSec  float64 `json:"miBPerSec"`
}

// meter accumulates work as a phase does it and publishes the rate on a timer.
// Phases that run as a single statement have nothing to sample mid-flight, so
// they report one final reading instead (see addPhase).
type meter struct {
	rows  atomic.Int64
	bytes atomic.Int64
	stop  func()
}

func (m *meter) add(rows, bytes int64) {
	m.rows.Add(rows)
	m.bytes.Add(bytes)
}

func (t *Tester) startMeter(phase string, runStart time.Time) *meter {
	m := &meter{}
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		var lastRows, lastBytes int64
		last := time.Now()
		for {
			select {
			case <-done:
				return
			case now := <-tick.C:
				rows, bytes := m.rows.Load(), m.bytes.Load()
				elapsed := now.Sub(last).Seconds()
				last = now
				if elapsed <= 0 {
					continue
				}
				t.emit(Sample{
					Phase:      phase,
					TMs:        float64(now.Sub(runStart).Milliseconds()),
					RowsPerSec: float64(rows-lastRows) / elapsed,
					MiBPerSec:  float64(bytes-lastBytes) / (1 << 20) / elapsed,
				})
				lastRows, lastBytes = rows, bytes
			}
		}
	}()
	m.stop = func() { close(done) }
	return m
}

// payloadAlphabet avoids quotes and backslashes so the driver's escaping does
// not inflate the statement, keeping measured bytes close to bytes on the wire.
const payloadAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 "

func randPayload(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = byte(i)
		}
	}
	for i, b := range buf {
		buf[i] = payloadAlphabet[int(b)%len(payloadAlphabet)]
	}
	return string(buf)
}

// SpeedTest inserts, reads, updates, commits and deletes real rows in a
// throwaway table, so the numbers reflect the whole client-server path rather
// than a synthetic ping.
// Named results so the deferred TotalMs assignment lands in what the caller gets.
func (t *Tester) SpeedTest(opts SpeedOptions) (res SpeedResult, err error) {
	rows := def(opts.Rows, 5000)
	payload := def(opts.PayloadBytes, 256)
	batch := def(opts.BatchSize, 200)
	txCount := def(opts.Transactions, 100)
	workers := def(opts.Concurrency, 1)
	if workers > 32 {
		workers = 32
	}

	res = SpeedResult{Phases: []Phase{}, Warnings: []string{}}
	if opts.Config.Database == "" {
		return res, fmt.Errorf("select a database first: the speed test needs somewhere to create its table")
	}

	// Generous budget: this is bounded by explicit work, and the user can cancel.
	ctx, done := t.begin(30 * time.Minute)
	defer done()
	overall := time.Now()
	defer func() { res.TotalMs = msSince(overall) }()

	db, err := opts.Config.open(ctx, workers)
	if err != nil {
		return res, fmt.Errorf("%s", errString(err))
	}
	defer db.Close()

	// Keep the batch inside max_allowed_packet with room for the statement itself.
	if maxPacket := statusVarInt(ctx, db, "max_allowed_packet"); maxPacket > 0 {
		if perRow := int64(payload + 48); perRow*int64(batch) > maxPacket*7/10 {
			batch = int(maxPacket * 7 / 10 / perRow)
			if batch < 1 {
				batch = 1
			}
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("Batch size reduced to %d rows to stay inside max_allowed_packet (%s).", batch, humanBytes(maxPacket)))
		}
	}
	if batch > rows {
		batch = rows
	}

	table := fmt.Sprintf("conntest_%d", time.Now().UnixNano()%1e10)
	res.Table = table
	// Identifier is generated above and never user input, so plain interpolation is safe.
	qname := "`" + table + "`"

	phases := 8
	addPhase := func(p Phase) {
		res.Phases = append(res.Phases, p)
		t.progress("speedtest", p.Name, float64(len(res.Phases))/float64(phases)*100)
		// One closing reading per phase, so single-statement phases still show
		// up on the chart and every phase ends on its own average.
		t.emit(Sample{
			Phase:      p.Key,
			TMs:        msSince(overall),
			RowsPerSec: p.RowsPerSec,
			MiBPerSec:  p.MiBPerSec,
		})
	}

	// 1. Create.
	start := time.Now()
	ddl := "CREATE TABLE " + qname + ` (
		id BIGINT NOT NULL AUTO_INCREMENT,
		k INT NOT NULL,
		payload LONGTEXT NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (id),
		KEY idx_k (k)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		addPhase(Phase{Name: "Create table", Key: "create", DurationMs: msSince(start), Error: errString(err), Hint: hint(err)})
		return res, nil
	}
	defer func() {
		if !opts.KeepTable {
			// Cleanup must survive a cancelled run, or we litter the user's database.
			dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			db.ExecContext(dropCtx, "DROP TABLE IF EXISTS "+qname)
		}
	}()
	engine := tableEngine(ctx, db, table)
	addPhase(Phase{Name: "Create table", Key: "create", OK: true, DurationMs: msSince(start), Detail: table + " (" + engine + ")"})
	if !strings.EqualFold(engine, "InnoDB") {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("Table was created as %s, not InnoDB. Transaction results below are meaningless on a non-transactional engine.", engine))
	}

	// 2. Bulk insert.
	insert := t.insertPhase(ctx, db, qname, rows, payload, batch, workers, overall)
	addPhase(insert)
	if !insert.OK {
		return res, nil
	}

	// 3. Full scan: streaming read throughput.
	addPhase(t.scanPhase(ctx, db, qname, overall))

	// 4. Point lookups: read latency rather than bandwidth.
	addPhase(t.pointPhase(ctx, db, qname, min(rows, 200), overall))

	// 5. Update.
	addPhase(t.updatePhase(ctx, db, qname, payload))

	// 6. Transactions.
	addPhase(t.txPhase(ctx, db, qname, txCount, payload, overall))

	// 7. Rollback correctness.
	addPhase(rollbackPhase(ctx, db, qname))

	// 8. Delete.
	addPhase(t.deletePhase(ctx, db, qname))

	res.OK = true
	for _, p := range res.Phases {
		if !p.OK {
			res.OK = false
		}
	}
	res.Summary, res.Warnings = speedSummary(res.Phases, res.Warnings)
	return res, nil
}

func (t *Tester) insertPhase(ctx context.Context, db *sql.DB, qname string, rows, payload, batch, workers int, since time.Time) Phase {
	m := t.startMeter("insert", since)
	defer m.stop()
	p := Phase{Key: "insert", Name: fmt.Sprintf("Insert %d rows x %s (%d/batch, %d conn)", rows, humanBytes(int64(payload)), batch, workers)}
	row := randPayload(payload)

	batches := (rows + batch - 1) / batch
	jobs := make(chan int, batches)
	for i := 0; i < batches; i++ {
		n := batch
		if i == batches-1 && rows%batch != 0 {
			n = rows % batch
		}
		jobs <- n
	}
	close(jobs)

	var inserted, doneBatches int64
	var firstErr atomic.Value
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range jobs {
				if ctx.Err() != nil || firstErr.Load() != nil {
					return
				}
				var b strings.Builder
				b.Grow(n * (payload + 32))
				b.WriteString("INSERT INTO " + qname + " (k, payload) VALUES ")
				for i := 0; i < n; i++ {
					if i > 0 {
						b.WriteByte(',')
					}
					fmt.Fprintf(&b, "(%d,?)", i)
				}
				args := make([]any, n)
				for i := range args {
					args[i] = row
				}
				if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
					firstErr.Store(err)
					return
				}
				atomic.AddInt64(&inserted, int64(n))
				m.add(int64(n), int64(n)*int64(payload))
				d := atomic.AddInt64(&doneBatches, 1)
				t.progress("speedtest", "inserting", float64(d)/float64(batches)*100)
			}
		}()
	}
	wg.Wait()
	p.DurationMs = msSince(start)
	p.Rows = atomic.LoadInt64(&inserted)
	p.Bytes = p.Rows * int64(payload)
	if v := firstErr.Load(); v != nil {
		err := v.(error)
		p.Error, p.Hint = errString(err), hint(err)
		return p
	}
	if err := ctx.Err(); err != nil {
		p.Error = "cancelled"
		return p
	}
	p.OK = true
	rate(&p)
	return p
}

func (t *Tester) scanPhase(ctx context.Context, db *sql.DB, qname string, since time.Time) Phase {
	m := t.startMeter("scan", since)
	defer m.stop()
	p := Phase{Key: "scan", Name: "Select: full table scan"}
	start := time.Now()
	rs, err := db.QueryContext(ctx, "SELECT id, k, payload FROM "+qname)
	if err != nil {
		p.DurationMs, p.Error, p.Hint = msSince(start), errString(err), hint(err)
		return p
	}
	defer rs.Close()
	var id int64
	var k int
	var payload string
	for rs.Next() {
		if err := rs.Scan(&id, &k, &payload); err != nil {
			p.DurationMs, p.Error = msSince(start), errString(err)
			return p
		}
		p.Rows++
		p.Bytes += int64(len(payload))
		m.add(1, int64(len(payload)))
	}
	if err := rs.Err(); err != nil {
		p.DurationMs, p.Error = msSince(start), errString(err)
		return p
	}
	p.DurationMs = msSince(start)
	p.OK = true
	rate(&p)
	return p
}

func (t *Tester) pointPhase(ctx context.Context, db *sql.DB, qname string, n int, since time.Time) Phase {
	m := t.startMeter("point lookups", since)
	defer m.stop()
	p := Phase{Key: "point lookups", Name: fmt.Sprintf("Select: %d point lookups by primary key", n)}
	var ids []int64
	rs, err := db.QueryContext(ctx, "SELECT id FROM "+qname+" ORDER BY id LIMIT ?", n)
	if err != nil {
		p.Error, p.Hint = errString(err), hint(err)
		return p
	}
	for rs.Next() {
		var id int64
		if err := rs.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rs.Close()

	samples := make([]float64, 0, len(ids))
	start := time.Now()
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		op := time.Now()
		var payload string
		if err := db.QueryRowContext(ctx, "SELECT payload FROM "+qname+" WHERE id = ?", id).Scan(&payload); err != nil {
			p.DurationMs, p.Error = msSince(start), errString(err)
			return p
		}
		samples = append(samples, msSince(op))
		p.Rows++
		p.Bytes += int64(len(payload))
		m.add(1, int64(len(payload)))
	}
	p.DurationMs = msSince(start)
	p.Stats = summarize(samples)
	p.OK = true
	rate(&p)
	p.OpsPerSec = p.RowsPerSec
	p.Detail = fmt.Sprintf("median %.2f ms, p99 %.2f ms", p.Stats.P50, p.Stats.P99)
	return p
}

func (t *Tester) updatePhase(ctx context.Context, db *sql.DB, qname string, payload int) Phase {
	p := Phase{Key: "update", Name: "Update: rewrite every payload"}
	start := time.Now()
	r, err := db.ExecContext(ctx, "UPDATE "+qname+" SET payload = ?", randPayload(payload))
	if err != nil {
		p.DurationMs, p.Error, p.Hint = msSince(start), errString(err), hint(err)
		return p
	}
	p.DurationMs = msSince(start)
	p.Rows, _ = r.RowsAffected()
	p.Bytes = p.Rows * int64(payload)
	p.OK = true
	rate(&p)
	return p
}

func (t *Tester) txPhase(ctx context.Context, db *sql.DB, qname string, n, payload int, since time.Time) Phase {
	m := t.startMeter("transactions", since)
	defer m.stop()
	p := Phase{Key: "transactions", Name: fmt.Sprintf("Transactions: %d insert+update+commit cycles", n)}
	row := randPayload(payload)
	samples := make([]float64, 0, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			break
		}
		op := time.Now()
		err := func() error {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			r, err := tx.ExecContext(ctx, "INSERT INTO "+qname+" (k, payload) VALUES (?, ?)", 999, row)
			if err != nil {
				return err
			}
			id, err := r.LastInsertId()
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE "+qname+" SET k = k + 1 WHERE id = ?", id); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err != nil {
			p.DurationMs, p.Error, p.Hint = msSince(start), errString(err), hint(err)
			p.Stats = summarize(samples)
			return p
		}
		samples = append(samples, msSince(op))
		p.Rows++
		m.add(1, int64(payload))
		if i%10 == 0 {
			t.progress("speedtest", "transactions", float64(i)/float64(n)*100)
		}
	}
	p.DurationMs = msSince(start)
	p.Stats = summarize(samples)
	p.OK = true
	if p.DurationMs > 0 {
		p.OpsPerSec = float64(p.Rows) / (p.DurationMs / 1000)
	}
	p.Detail = fmt.Sprintf("%.1f commits/s, median %.2f ms", p.OpsPerSec, p.Stats.P50)
	return p
}

// rollbackPhase is a correctness check, not a benchmark: a rolled back insert
// must leave nothing behind. It catches a table that silently is not InnoDB.
func rollbackPhase(ctx context.Context, db *sql.DB, qname string) Phase {
	p := Phase{Key: "rollback", Name: "Transactions: rollback is honoured"}
	const sentinel = 424242
	start := time.Now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		p.DurationMs, p.Error, p.Hint = msSince(start), errString(err), hint(err)
		return p
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+qname+" (k, payload) VALUES (?, 'rollback')", sentinel); err != nil {
		tx.Rollback()
		p.DurationMs, p.Error = msSince(start), errString(err)
		return p
	}
	if err := tx.Rollback(); err != nil {
		p.DurationMs, p.Error = msSince(start), errString(err)
		return p
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qname+" WHERE k = ?", sentinel).Scan(&n); err != nil {
		p.DurationMs, p.Error = msSince(start), errString(err)
		return p
	}
	p.DurationMs = msSince(start)
	if n != 0 {
		p.Error = fmt.Sprintf("rolled back row is still present (%d found)", n)
		p.Hint = "The table is not transactional, or autocommit is being forced by a proxy. Do not rely on transactions against this server."
		return p
	}
	p.OK = true
	p.Detail = "rolled back row is gone, as expected"
	return p
}

func (t *Tester) deletePhase(ctx context.Context, db *sql.DB, qname string) Phase {
	p := Phase{Key: "delete", Name: "Delete: empty the table"}
	start := time.Now()
	r, err := db.ExecContext(ctx, "DELETE FROM "+qname)
	if err != nil {
		p.DurationMs, p.Error, p.Hint = msSince(start), errString(err), hint(err)
		return p
	}
	p.DurationMs = msSince(start)
	p.Rows, _ = r.RowsAffected()
	p.OK = true
	rate(&p)
	return p
}

func rate(p *Phase) {
	if p.DurationMs <= 0 {
		return
	}
	sec := p.DurationMs / 1000
	p.RowsPerSec = float64(p.Rows) / sec
	p.MiBPerSec = float64(p.Bytes) / (1 << 20) / sec
}

func tableEngine(ctx context.Context, db *sql.DB, table string) string {
	var engine string
	err := db.QueryRowContext(ctx,
		"SELECT ENGINE FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?", table).Scan(&engine)
	if err != nil {
		return "unknown"
	}
	return engine
}

func statusVarInt(ctx context.Context, db *sql.DB, name string) int64 {
	var n int64
	// Name comes from a fixed internal list, never user input.
	if err := db.QueryRowContext(ctx, "SELECT @@"+name).Scan(&n); err != nil {
		return 0
	}
	return n
}

func speedSummary(phases []Phase, warns []string) (string, []string) {
	var insert, scan Phase
	for _, p := range phases {
		switch {
		case strings.HasPrefix(p.Name, "Insert"):
			insert = p
		case p.Name == "Select: full table scan":
			scan = p
		}
	}
	if !insert.OK || !scan.OK {
		return "Run did not complete; see the failing phase.", warns
	}
	if scan.MiBPerSec > 0 && insert.MiBPerSec > 0 && scan.MiBPerSec < insert.MiBPerSec/3 {
		warns = append(warns, "Reads are much slower than writes, which usually means the result set is being fetched row by row over a high-latency link.")
	}
	if insert.MiBPerSec < 1 {
		warns = append(warns, "Write throughput is under 1 MiB/s. Expect bulk loads and migrations against this server to be painful.")
	}
	return fmt.Sprintf("Write %.2f MiB/s (%.0f rows/s), read %.2f MiB/s (%.0f rows/s).",
		insert.MiBPerSec, insert.RowsPerSec, scan.MiBPerSec, scan.RowsPerSec), warns
}

func def(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}
