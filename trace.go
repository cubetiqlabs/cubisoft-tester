package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// TraceOptions controls one traceroute run.
type TraceOptions struct {
	Config Config `json:"config"`
	// MaxHops caps the TTL. 0 means 30.
	MaxHops int `json:"maxHops"`
	// Probes per hop. 0 means 3.
	Probes int `json:"probes"`
	// TimeoutMs to wait for each probe. 0 means 1000.
	TimeoutMs int `json:"timeoutMs"`
	// ResolveNames does reverse DNS on each hop.
	ResolveNames bool `json:"resolveNames"`
}

// Hop is one TTL step on the path.
type Hop struct {
	TTL   int       `json:"ttl"`
	Addr  string    `json:"addr"`
	Host  string    `json:"host"`
	RTTs  []float64 `json:"rtts"` // ms; -1 means no reply
	AvgMs float64   `json:"avgMs"`
	Final bool      `json:"final"`
	Note  string    `json:"note"`
}

// TraceResult is the whole path plus a check that the MySQL port itself answers.
type TraceResult struct {
	Target     string  `json:"target"`
	TargetIP   string  `json:"targetIp"`
	Mode       string  `json:"mode"`
	Hops       []Hop   `json:"hops"`
	Reached    bool    `json:"reached"`
	PortOpen   bool    `json:"portOpen"`
	PortMs     float64 `json:"portMs"`
	PortError  string  `json:"portError"`
	Note       string  `json:"note"`
	TotalHops  int     `json:"totalHops"`
	LongestHop int     `json:"longestHop"` // TTL of the biggest latency jump
}

// Trace walks the path to the database host with TTL-limited ICMP echoes, then
// confirms the last mile by opening a real TCP connection to the MySQL port.
// A hop showing * is a router that declines to answer, not necessarily a fault.
func (t *Tester) Trace(opts TraceOptions) (TraceResult, error) {
	maxHops := def(opts.MaxHops, 30)
	probes := def(opts.Probes, 3)
	timeout := time.Duration(def(opts.TimeoutMs, 1000)) * time.Millisecond

	res := TraceResult{Target: opts.Config.Host, Hops: []Hop{}}

	ctx, done := t.begin(time.Duration(maxHops*probes)*timeout + 30*time.Second)
	defer done()

	ipAddr, err := net.ResolveIPAddr("ip4", opts.Config.Host)
	if err != nil {
		if _, err6 := net.ResolveIPAddr("ip6", opts.Config.Host); err6 == nil {
			// ponytail: IPv4 only. Add an ipv6.PacketConn branch if v6-only servers show up.
			return res, fmt.Errorf("%s resolves to IPv6 only; this trace supports IPv4", opts.Config.Host)
		}
		return res, fmt.Errorf("cannot resolve %s: %w", opts.Config.Host, err)
	}
	res.TargetIP = ipAddr.IP.String()

	// The TCP check runs regardless of whether ICMP works, because it is the
	// one that actually answers "can I reach the database".
	res.PortOpen, res.PortMs, res.PortError = tcpProbe(opts.Config)

	conn, mode, err := listenICMP()
	if err != nil {
		res.Mode = "unavailable"
		res.Note = "ICMP is not available on this machine: " + err.Error() +
			". The TCP port check below still applies."
		return res, nil
	}
	defer conn.Close()
	res.Mode = mode

	p := conn.IPv4PacketConn()
	if err := p.SetControlMessage(ipv4.FlagTTL|ipv4.FlagSrc, true); err != nil {
		// Not fatal: control messages are a nicety, the reply source address is enough.
		_ = err
	}

	seqBase := rand.Intn(30000)
	for ttl := 1; ttl <= maxHops; ttl++ {
		if ctx.Err() != nil {
			break
		}
		hop := Hop{TTL: ttl, RTTs: []float64{}}
		for i := 0; i < probes; i++ {
			seq := (seqBase + ttl*16 + i) & 0xffff
			rtt, from, final, err := probeOnce(p, ipAddr, ttl, seq, timeout)
			switch {
			case err != nil:
				hop.RTTs = append(hop.RTTs, -1)
				if hop.Note == "" && !errors.Is(err, os.ErrDeadlineExceeded) {
					hop.Note = err.Error()
				}
			default:
				hop.RTTs = append(hop.RTTs, rtt)
				if hop.Addr == "" {
					hop.Addr = from
				}
				if final {
					hop.Final = true
				}
			}
		}
		hop.AvgMs = summarize(okSamples(hop.RTTs)).Avg
		if hop.Addr != "" && opts.ResolveNames {
			hop.Host = reverseDNS(ctx, hop.Addr)
		}
		res.Hops = append(res.Hops, hop)
		t.progress("trace", fmt.Sprintf("hop %d", ttl), float64(ttl)/float64(maxHops)*100)
		if hop.Final {
			res.Reached = true
			break
		}
	}

	res.TotalHops = len(res.Hops)
	res.LongestHop = biggestJump(res.Hops)
	if !res.Reached && res.Mode != "unavailable" {
		res.Note = "The destination never replied to ICMP. Many servers and cloud firewalls drop echo requests, so this alone is not a fault; trust the TCP port check."
	}
	return res, nil
}

// listenICMP prefers the unprivileged datagram socket, which works as a normal
// user on macOS and on Linux where ping_group_range allows it. Raw ICMP is the
// fallback and needs root or Administrator.
func listenICMP() (*icmp.PacketConn, string, error) {
	if c, err := icmp.ListenPacket("udp4", "0.0.0.0"); err == nil {
		return c, "icmp-datagram", nil
	}
	c, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, "", err
	}
	return c, "icmp-raw", nil
}

// probeOnce sends one TTL-limited echo and waits for the router that drops it
// (time exceeded) or for the destination itself (echo reply).
func probeOnce(p *ipv4.PacketConn, dst *net.IPAddr, ttl, seq int, timeout time.Duration) (float64, string, bool, error) {
	if err := p.SetTTL(ttl); err != nil {
		return 0, "", false, err
	}
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Body: &icmp.Echo{ID: os.Getpid() & 0xffff, Seq: seq, Data: []byte("wails-mysql-trace")},
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return 0, "", false, err
	}

	start := time.Now()
	// In datagram mode the kernel wants a UDPAddr; in raw mode an IPAddr.
	var target net.Addr = dst
	if p.PacketConn.LocalAddr().Network() == "udp" {
		target = &net.UDPAddr{IP: dst.IP}
	}
	if _, err := p.WriteTo(wire, nil, target); err != nil {
		return 0, "", false, err
	}

	deadline := time.Now().Add(timeout)
	buf := make([]byte, 1500)
	for {
		if err := p.SetReadDeadline(deadline); err != nil {
			return 0, "", false, err
		}
		n, _, peer, err := p.ReadFrom(buf)
		if err != nil {
			return 0, "", false, err
		}
		parsed, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), buf[:n])
		if err != nil {
			continue
		}
		from := addrIP(peer)
		switch body := parsed.Body.(type) {
		case *icmp.Echo:
			// Our own reply from the destination. The ID is rewritten by the
			// kernel in datagram mode, so sequence is the only reliable match.
			if parsed.Type == ipv4.ICMPTypeEchoReply && body.Seq == seq {
				return msSince(start), from, true, nil
			}
		case *icmp.TimeExceeded:
			if embeddedSeq(body.Data) == seq {
				return msSince(start), from, false, nil
			}
		case *icmp.DstUnreach:
			if embeddedSeq(body.Data) == seq {
				return msSince(start), from, true, nil
			}
		}
		// Someone else's ICMP traffic; keep waiting until the deadline.
	}
}

// embeddedSeq digs the original echo sequence out of the quoted packet that
// routers return inside a time-exceeded message.
func embeddedSeq(data []byte) int {
	hdr, err := icmp.ParseIPv4Header(data)
	if err != nil || len(data) < hdr.Len+8 {
		return -1
	}
	inner, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), data[hdr.Len:])
	if err != nil {
		return -1
	}
	if echo, ok := inner.Body.(*icmp.Echo); ok {
		return echo.Seq
	}
	return -1
}

func addrIP(a net.Addr) string {
	switch v := a.(type) {
	case *net.UDPAddr:
		return v.IP.String()
	case *net.IPAddr:
		return v.IP.String()
	}
	return a.String()
}

func reverseDNS(ctx context.Context, ip string) string {
	ctx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	names, err := net.DefaultResolver.LookupAddr(ctx, ip)
	if err != nil || len(names) == 0 {
		return ""
	}
	return names[0]
}

func tcpProbe(cfg Config) (bool, float64, string) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", cfg.addr(), cfg.timeout())
	if err != nil {
		return false, msSince(start), errString(err)
	}
	conn.Close()
	return true, msSince(start), ""
}

// biggestJump returns the TTL where latency grew the most, which is usually
// the long-haul link (an ocean crossing, or the hop into the cloud region).
func biggestJump(hops []Hop) int {
	var prev, best float64
	ttl := 0
	for _, h := range hops {
		if h.AvgMs <= 0 {
			continue
		}
		if prev > 0 && h.AvgMs-prev > best {
			best, ttl = h.AvgMs-prev, h.TTL
		}
		prev = h.AvgMs
	}
	return ttl
}
