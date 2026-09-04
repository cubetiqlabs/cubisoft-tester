package main

import (
	"context"
	"net"
	"time"
)

// LatencyOptions controls one latency run.
type LatencyOptions struct {
	Config Config `json:"config"`
	// Count is how many probes to send per series. 0 means 20.
	Count int `json:"count"`
	// IntervalMs paces the probes so a burst does not measure queueing. 0 means 100.
	IntervalMs int `json:"intervalMs"`
	// ColdConnects, if > 0, additionally measures full connect+auth+close cycles.
	ColdConnects int `json:"coldConnects"`
}

// Series is one set of probes of a single kind.
type Series struct {
	Name    string    `json:"name"`
	Samples []float64 `json:"samples"` // ms; -1 marks a failed probe
	Sent    int       `json:"sent"`
	Lost    int       `json:"lost"`
	Stats   Stats     `json:"stats"`
	Error   string    `json:"error"`
}

// LatencyResult holds every series from one run.
type LatencyResult struct {
	Series   []Series `json:"series"`
	Verdict  string   `json:"verdict"`
	Warnings []string `json:"warnings"`
}

// Latency measures three different things people all call "latency":
// the TCP round trip, a query on an already open connection, and the cost of
// opening a brand new connection. Pool sizing depends on the gap between them.
func (t *Tester) Latency(opts LatencyOptions) (LatencyResult, error) {
	count := opts.Count
	if count <= 0 {
		count = 20
	}
	interval := time.Duration(opts.IntervalMs) * time.Millisecond
	if opts.IntervalMs <= 0 {
		interval = 100 * time.Millisecond
	}
	cold := opts.ColdConnects
	if cold < 0 {
		cold = 0
	}

	budget := time.Duration(count)*(interval+opts.Config.timeout()) +
		time.Duration(cold)*opts.Config.timeout() + 30*time.Second
	ctx, done := t.begin(budget)
	defer done()

	res := LatencyResult{Series: []Series{}, Warnings: []string{}}
	total := float64(count*2 + cold)
	var stepsDone float64
	tick := func(phase string) {
		stepsDone++
		t.progress("latency", phase, stepsDone/total*100)
	}

	// TCP handshake only: the network floor, with no MySQL in the picture.
	tcp := Series{Name: "TCP connect", Samples: []float64{}}
	for i := 0; i < count && ctx.Err() == nil; i++ {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", opts.Config.addr(), opts.Config.timeout())
		if err != nil {
			tcp.Samples = append(tcp.Samples, -1)
			tcp.Lost++
			if tcp.Error == "" {
				tcp.Error = errString(err)
			}
		} else {
			tcp.Samples = append(tcp.Samples, msSince(start))
			conn.Close()
		}
		tcp.Sent++
		tick("TCP handshake")
		sleepCtx(ctx, interval)
	}
	tcp.Stats = summarize(okSamples(tcp.Samples))
	res.Series = append(res.Series, tcp)

	// Query on a pinned connection: what an application with a warm pool sees.
	db, err := opts.Config.open(ctx, 1)
	if err != nil {
		res.Series = append(res.Series, Series{Name: "Query round trip", Samples: []float64{}, Error: errString(err)})
		res.Verdict = "Could not open a MySQL connection: " + errString(err)
		return res, nil
	}
	defer db.Close()

	conn, err := db.Conn(ctx)
	if err != nil {
		return res, err
	}
	q := Series{Name: "Query round trip", Samples: []float64{}}
	for i := 0; i < count && ctx.Err() == nil; i++ {
		start := time.Now()
		var one int
		err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&one)
		if err != nil {
			q.Samples = append(q.Samples, -1)
			q.Lost++
			if q.Error == "" {
				q.Error = errString(err)
			}
		} else {
			q.Samples = append(q.Samples, msSince(start))
		}
		q.Sent++
		tick("SELECT 1")
		sleepCtx(ctx, interval)
	}
	conn.Close()
	q.Stats = summarize(okSamples(q.Samples))
	res.Series = append(res.Series, q)

	// Cold connect: TCP + TLS + auth + close, i.e. what every request pays when
	// the pool is too small or connections are being churned.
	if cold > 0 {
		c := Series{Name: "Cold connect (TCP+auth)", Samples: []float64{}}
		for i := 0; i < cold && ctx.Err() == nil; i++ {
			start := time.Now()
			fresh, err := opts.Config.open(ctx, 1)
			if err != nil {
				c.Samples = append(c.Samples, -1)
				c.Lost++
				if c.Error == "" {
					c.Error = errString(err)
				}
			} else {
				c.Samples = append(c.Samples, msSince(start))
				fresh.Close()
			}
			c.Sent++
			tick("cold connect")
			sleepCtx(ctx, interval)
		}
		c.Stats = summarize(okSamples(c.Samples))
		res.Series = append(res.Series, c)
	}

	res.Verdict, res.Warnings = latencyVerdict(res.Series)
	return res, nil
}

func okSamples(s []float64) []float64 {
	out := make([]float64, 0, len(s))
	for _, v := range s {
		if v >= 0 {
			out = append(out, v)
		}
	}
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func latencyVerdict(series []Series) (string, []string) {
	warns := []string{}
	byName := map[string]Series{}
	for _, s := range series {
		byName[s.Name] = s
	}
	tcp, q, cold := byName["TCP connect"], byName["Query round trip"], byName["Cold connect (TCP+auth)"]

	if q.Sent == 0 || q.Sent == q.Lost {
		return "No successful queries.", warns
	}
	if q.Lost > 0 {
		warns = append(warns, "Queries failed mid-run. An intermittent path is worse than a slow one: retries will mask it until they do not.")
	}
	if tcp.Sent > 0 && tcp.Lost > 0 {
		warns = append(warns, "Some TCP handshakes failed. Packet loss or a connection-rate limit on the path.")
	}
	if q.Stats.Jitter > q.Stats.P50 {
		warns = append(warns, "Jitter exceeds the median round trip. The path is unstable, so tail latency will be unpredictable.")
	}
	if q.Stats.P99 > 3*q.Stats.P50 && q.Stats.P50 > 0 {
		warns = append(warns, "p99 is more than three times the median: something intermittently stalls, often a busy server or a saturated link.")
	}
	if cold.Sent > 0 && cold.Stats.Avg > 3*q.Stats.Avg && q.Stats.Avg > 0 {
		warns = append(warns, "Opening a connection costs far more than a query. Keep a warm pool; do not connect per request.")
	}
	if tcp.Stats.Avg > 0 && q.Stats.Avg > 2*tcp.Stats.Avg && q.Stats.Avg-tcp.Stats.Avg > 5 {
		warns = append(warns, "Queries cost noticeably more than a bare TCP round trip, so the delay is on the server, not the network.")
	}

	var verdict string
	p50 := q.Stats.P50
	switch {
	case p50 < 1:
		verdict = "Excellent: same host or same rack."
	case p50 < 5:
		verdict = "Good: same datacentre. Chatty query patterns are fine here."
	case p50 < 30:
		verdict = "Fair: same region. Batch your queries; avoid N+1 patterns."
	case p50 < 100:
		verdict = "Slow: cross-region. Every round trip is expensive, so move logic into fewer, larger queries."
	default:
		verdict = "Very slow: intercontinental or a congested link. Consider a read replica closer to this client."
	}
	return verdict, warns
}
