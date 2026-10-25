// Package accesslog reads the access log of the Xray that 3x-ui runs and
// passes on, line by line, who a connection Xray accepted belongs to and what
// it asked for. A connection is known there by the inbound port it arrived on
// and the client address and port Xray accepted it from; behind a relay that
// is the relay's address, one per relayed connection. A line names its
// inbound by tag, which the panel's inbounds turn into a port: 3x-ui adds an
// inbound to the running Xray without rewriting the configuration Xray
// started with. The package keeps no connections: internal/xray/ledger
// matches the lines to the kernel's.
//
// 3x-ui appends the log to a copy and truncates it in place, hourly or every
// ten seconds while a client exceeds its IP limit, and Xray appends, so the
// file starts again at zero. The lines not read before a truncation are read
// back from the copy.
package accesslog

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	pollInterval = time.Second
	locateEvery  = 5 * time.Minute
	locateRetry  = 10 * time.Second
	// A line naming an inbound the panel did not list rereads the list at most
	// this often; a tag the panel still lacks waits for the next scheduled read.
	relistEvery = 10 * time.Second
	listTimeout = 10 * time.Second
	// A line read while following the log is at most this old. A line that
	// seems older, or later than now, was stamped on another clock or in
	// another time zone, and counts as written when it was read.
	maxDelay = time.Minute
	// persistentCopy is where 3x-ui appends the log before truncating it.
	persistentCopy = "/var/log/x-ui/3xipl-ap.log"
)

// Conn is a connection as the access log names it: the inbound port it
// arrived on and the client address it came from.
type Conn struct {
	Port   uint16
	Remote netip.AddrPort
}

// Line is one access log line: Xray accepted a request of Email on Conn for
// Dest and sent it out through Outbound.
type Line struct {
	Conn  Conn
	Email string
	// At is when the line counts as written for finding its connection: the
	// time it carries, or when it was read when that time is off. It is zero
	// for an old line without a readable time.
	At time.Time
	// Written is the time the line carries, zero when it has none.
	Written time.Time
	// Backlog is set for a line read on opening the log, which an earlier
	// process may have read already.
	Backlog bool
	// UDP is set when the line may belong to a UDP flow rather than a TCP
	// connection: the inbound takes UDP, and the line is new or the inbound
	// takes nothing else. The log does not say which a line of an inbound
	// that takes both is for; an old one is most likely a TCP connection
	// closed long ago, and as a UDP flow it would stay limited for hours.
	UDP      bool
	Dest     Dest
	Outbound string
	// Blocked is set when Outbound is a blackhole in the running
	// configuration, whatever it is called.
	Blocked bool
}

// Dest is where a request asked to go. Host is empty when the line did not
// say in a form this package reads; Network is "tcp" or "udp", or empty when
// the line does not say, as Shadowsocks 2022 lines never do.
type Dest struct {
	Network, Host string
	Port          uint16
}

// Inbound is one of the panel's inbounds: the tag its lines carry, its port
// and protocol, the network its settings name (such as Shadowsocks'
// "tcp,udp") and the network of its transport.
type Inbound struct {
	Tag, Protocol      string
	Port               int
	Network, Transport string
}

// Inbounds lists the panel's inbounds.
type Inbounds func(context.Context) ([]Inbound, error)

// inbound is what a tag stands for.
type inbound struct {
	port uint16
	// tcp and udp are how clients can reach the inbound. The kernel's TCP
	// table does not show UDP.
	tcp, udp bool
}

// index maps each inbound's tag to its port and how clients reach it.
func index(list []Inbound) map[string]inbound {
	inbounds := map[string]inbound{}
	for _, ib := range list {
		if ib.Tag == "" || ib.Port <= 0 || ib.Port > 65535 {
			continue
		}
		udpOnly := strings.HasPrefix(ib.Protocol, "hysteria") || ib.Transport == "kcp" || ib.Transport == "quic"
		tcp, udp := !udpOnly, udpOnly
		if ib.Network != "" && !udpOnly {
			tcp, udp = strings.Contains(ib.Network, "tcp"), strings.Contains(ib.Network, "udp")
		}
		inbounds[ib.Tag] = inbound{port: uint16(ib.Port), tcp: tcp, udp: udp}
	}
	return inbounds
}

// Log follows the access log.
type Log struct {
	proc, copy string
	list       Inbounds
	now        func() time.Time

	mu  sync.Mutex
	err error
}

func New(list Inbounds) *Log { return newLog("/proc", persistentCopy, list) }

func newLog(proc, copy string, list Inbounds) *Log {
	return &Log{proc: proc, copy: copy, list: list, now: time.Now, err: errors.New("xray access log not read yet")}
}

// Err reports why the log is not being followed, or nil while it is.
func (l *Log) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *Log) setErr(err error) {
	l.mu.Lock()
	l.err = err
	l.mu.Unlock()
}

// Run follows the log until ctx ends and passes each poll's lines to emit, in
// the order they were written. On opening the log it first passes the lines
// already there, starting with the end of 3x-ui's copy. Losing the log is
// not fatal: Err says why until the log is found again.
func (l *Log) Run(ctx context.Context, emit func([]Line)) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	var f *follower
	defer func() { f.close() }()
	var inbounds map[string]inbound
	var blackholes map[string]bool
	// unlisted holds the tags of lines the panel did not list when asked.
	unlisted := map[string]bool{}
	var locateAt, relistAt time.Time
	for {
		now := l.now()
		if !now.Before(locateAt) {
			found, err := locate(l.proc)
			if err == nil {
				// The lines already in the log are read on opening it, so the
				// inbounds must be known first; later a failed read keeps them.
				if found, listErr := l.inbounds(ctx); listErr == nil {
					inbounds, unlisted = found, map[string]bool{}
				} else if inbounds == nil {
					err = fmt.Errorf("3x-ui inbounds unavailable: %w", listErr)
				}
			}
			switch {
			case err != nil:
				f.close()
				f = nil
				l.setErr(err)
			case f == nil || f.path != found.log:
				f.close()
				f = &follower{path: found.log, copy: l.copy}
			}
			if err == nil {
				blackholes = found.blackholes
			}
			locateAt = now.Add(locateEvery)
			if f == nil {
				locateAt = now.Add(locateRetry)
			}
		}
		if f != nil {
			var entries []entry
			err := f.poll(func(raw []byte, backlog bool) {
				if e, ok := parse(raw); ok {
					e.backlog = backlog
					entries = append(entries, e)
				}
			})
			// An inbound added since the last read would lose its first lines,
			// and a long connection logs only once.
			if unknown(entries, inbounds, unlisted) && !now.Before(relistAt) {
				if found, err := l.inbounds(ctx); err == nil {
					inbounds = found
					for _, e := range entries {
						if _, listed := inbounds[e.tag]; !listed {
							unlisted[e.tag] = true
						}
					}
				}
				relistAt = now.Add(relistEvery)
			}
			if lines := l.lines(entries, inbounds, blackholes); len(lines) > 0 {
				emit(lines)
			}
			l.setErr(err)
			if err != nil {
				f.close()
				f, locateAt = nil, now.Add(locateRetry)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *Log) inbounds(ctx context.Context) (map[string]inbound, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	list, err := l.list(ctx)
	if err != nil {
		return nil, err
	}
	return index(list), nil
}

// unknown reports whether a line names an inbound neither listed nor found
// missing from the panel.
func unknown(entries []entry, inbounds map[string]inbound, unlisted map[string]bool) bool {
	for _, e := range entries {
		if _, found := inbounds[e.tag]; !found && !unlisted[e.tag] {
			return true
		}
	}
	return false
}

// lines passes on the entries of inbounds the panel lists.
func (l *Log) lines(entries []entry, inbounds map[string]inbound, blackholes map[string]bool) []Line {
	now := l.now()
	out := make([]Line, 0, len(entries))
	for _, e := range entries {
		ib, found := inbounds[e.tag]
		if !found {
			continue
		}
		at := e.at
		switch {
		case e.backlog && at.After(now):
			at = now
		case !e.backlog && (at.After(now) || now.Sub(at) > maxDelay):
			at = now
		}
		out = append(out, Line{Conn: Conn{ib.port, e.addr}, Email: e.email, At: at, Written: e.at, Backlog: e.backlog,
			UDP: ib.udp && !(e.backlog && ib.tcp), Dest: e.dest, Outbound: e.outbound, Blocked: blackholes[e.outbound]})
	}
	return out
}
