// Package accesslog reads the access log of the Xray that 3x-ui runs and
// passes on, line by line, who a connection Xray accepted belongs to. A
// connection is known there by the inbound port it arrived on and the client
// address and port Xray accepted it from; behind a relay that is the relay's
// address, one per relayed connection. The package keeps no connections:
// internal/modules/xui/ledger matches the lines to the kernel's. Nothing
// here leaves the process.
//
// 3x-ui appends the log to a copy and truncates it in place, hourly or every
// ten seconds while a client exceeds its IP limit, and Xray appends, so the
// file starts again at zero. The lines not read before a truncation are read
// back from the copy.
package accesslog

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"
)

const (
	pollInterval = time.Second
	locateEvery  = 5 * time.Minute
	locateRetry  = 10 * time.Second
	// A line naming an inbound the cached configuration lacks rereads the
	// configuration at most this often.
	relocateEvery = 10 * time.Second
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

// Claim is one access log line: Xray accepted a request of Email on Conn.
type Claim struct {
	Conn  Conn
	Email string
	// At is when the line was written. It is zero for an old line without
	// a readable time.
	At time.Time
	// UDP is set when the line may belong to a UDP flow rather than a TCP
	// connection: the inbound takes UDP, and the line is new or the inbound
	// takes nothing else. The log does not say which a line of an inbound
	// that takes both is for; an old one is most likely a TCP connection
	// closed long ago, and as a UDP flow it would stay limited for hours.
	UDP bool
}

// Log follows the access log.
type Log struct {
	proc, copy string
	now        func() time.Time

	mu  sync.Mutex
	err error
}

func New() *Log { return newLog("/proc", persistentCopy) }

func newLog(proc, copy string) *Log {
	return &Log{proc: proc, copy: copy, now: time.Now, err: errors.New("xray access log not read yet")}
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
func (l *Log) Run(ctx context.Context, emit func([]Claim)) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	var f *follower
	defer func() { f.close() }()
	var inbounds map[string]inbound
	var locateAt, relocateAt time.Time
	for {
		now := l.now()
		if !now.Before(locateAt) {
			path, found, err := locate(l.proc)
			switch {
			case err != nil:
				f.close()
				f = nil
				l.setErr(err)
			case f == nil || f.path != path:
				f.close()
				f = &follower{path: path, copy: l.copy}
			}
			if err == nil {
				inbounds = found
			}
			locateAt = now.Add(locateEvery)
			if f == nil {
				locateAt = now.Add(locateRetry)
			}
		}
		if f != nil {
			var lines []line
			err := f.poll(func(raw []byte, backlog bool) {
				if ln, ok := parse(raw); ok {
					ln.backlog = backlog
					lines = append(lines, ln)
				}
			})
			// An inbound added since the configuration was read would lose its
			// first lines, and a long connection logs only once.
			if unknown(lines, inbounds) && !now.Before(relocateAt) {
				if _, found, err := locate(l.proc); err == nil {
					inbounds = found
				}
				relocateAt = now.Add(relocateEvery)
			}
			if claims := l.claims(lines, inbounds); len(claims) > 0 {
				emit(claims)
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

// unknown reports whether a line names an inbound the configuration lacks.
func unknown(lines []line, inbounds map[string]inbound) bool {
	for _, ln := range lines {
		if _, found := inbounds[ln.tag]; !found {
			return true
		}
	}
	return false
}

// claims turns lines into claims, skipping those of inbounds the
// configuration does not name.
func (l *Log) claims(lines []line, inbounds map[string]inbound) []Claim {
	now := l.now()
	out := make([]Claim, 0, len(lines))
	for _, ln := range lines {
		ib, found := inbounds[ln.tag]
		if !found {
			continue
		}
		at := ln.at
		switch {
		case ln.backlog && at.After(now):
			at = now
		case !ln.backlog && (at.After(now) || now.Sub(at) > maxDelay):
			at = now
		}
		out = append(out, Claim{Conn: Conn{ib.port, ln.addr}, Email: ln.email, At: at, UDP: ib.udp && !(ln.backlog && ib.tcp)})
	}
	return out
}
