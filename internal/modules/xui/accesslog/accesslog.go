// Package accesslog follows the access log of the Xray that 3x-ui runs and
// keeps, in memory only, which user each connection belongs to. A connection
// is known by the client address and port Xray accepted it from, which is
// also where its download goes; behind a relay that is the relay's address,
// one per relayed connection. Nothing here leaves the process.
//
// 3x-ui copies the log aside and truncates it in place, hourly or every ten
// seconds when a client has an IP limit, and Xray appends, so the file starts
// again at zero. Reading every second loses at most the lines written in the
// second before a truncation; those connections simply stay unknown.
package accesslog

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"time"
)

const (
	pollInterval  = time.Second
	locateEvery   = 5 * time.Minute
	locateRetry   = 10 * time.Second
	pruneInterval = time.Minute
	// A connection is forgotten once no line named it for this long and the
	// kernel holds no TCP connection to its address. UDP flows have no socket
	// to look for, so a quiet one is forgotten after this time.
	keepUnseen = 2 * time.Hour
	maxEntries = 65536
	// persistentCopy is where 3x-ui appends the log before truncating it.
	persistentCopy = "/var/log/x-ui/3xipl-ap.log"
)

// Log is the connection table and the loop that keeps it current.
type Log struct {
	proc, copy string

	mu    sync.RWMutex
	conns map[netip.AddrPort]entry
	err   error
}

type entry struct {
	email string
	seen  time.Time
}

func New() *Log { return newLog("/proc") }

func newLog(proc string) *Log {
	return &Log{proc: proc, copy: persistentCopy, conns: map[netip.AddrPort]entry{}, err: errors.New("xray access log not read yet")}
}

// Err reports why the log is not being followed, or nil while it is.
func (l *Log) Err() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.err
}

// Connections returns the known connections of each named email.
func (l *Log) Connections(emails map[string]bool) map[string][]netip.AddrPort {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := map[string][]netip.AddrPort{}
	for addr, e := range l.conns {
		if emails[e.email] {
			out[e.email] = append(out[e.email], addr)
		}
	}
	return out
}

// Run follows the log until ctx ends. Losing the log is not fatal: the table
// keeps its connections and Err says why, until the log is found again.
func (l *Log) Run(ctx context.Context) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	var f *follower
	defer func() { f.close() }()
	var locateAt, pruneAt time.Time
	l.seed(time.Now())
	for {
		now := time.Now()
		if !now.Before(locateAt) {
			path, err := locate(l.proc)
			if err == nil && (f == nil || f.path != path) {
				f.close()
				f = &follower{path: path}
			}
			if err != nil {
				f.close()
				f = nil
				l.setErr(err)
			}
			locateAt = now.Add(locateEvery)
			if f == nil {
				locateAt = now.Add(locateRetry)
			}
		}
		if f != nil {
			var lines []line
			err := f.poll(func(raw []byte) {
				if addr, email, ok := parse(raw); ok {
					lines = append(lines, line{addr, email})
				}
			})
			l.add(lines, now)
			l.setErr(err)
			if err != nil {
				f.close()
				f, locateAt = nil, now.Add(locateRetry)
			}
		}
		if !now.Before(pruneAt) {
			live, err := liveTCP(l.proc)
			if err == nil {
				l.prune(now, live)
			}
			pruneAt = now.Add(pruneInterval)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// seed learns the TCP connections still open from the end of 3x-ui's copy,
// so connections opened before the last truncation are known too. The live
// log, read next, overrides it.
func (l *Log) seed(now time.Time) {
	live, err := liveTCP(l.proc)
	if err != nil {
		return
	}
	f := &follower{path: l.copy}
	defer f.close()
	var lines []line
	_ = f.poll(func(raw []byte) {
		if addr, email, ok := parse(raw); ok && live[addr] {
			lines = append(lines, line{addr, email})
		}
	})
	l.add(lines, now)
}

type line struct {
	addr  netip.AddrPort
	email string
}

func (l *Log) setErr(err error) {
	l.mu.Lock()
	l.err = err
	l.mu.Unlock()
}

// add records lines in order, so a reused address belongs to its newest user.
func (l *Log) add(lines []line, now time.Time) {
	if len(lines) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ln := range lines {
		l.conns[ln.addr] = entry{email: ln.email, seen: now}
	}
	if len(l.conns) > maxEntries {
		l.evictOldest(len(l.conns) - maxEntries*9/10)
	}
}

// evictOldest drops the n least recently seen connections; l.mu must be held.
func (l *Log) evictOldest(n int) {
	byAge := make([]netip.AddrPort, 0, len(l.conns))
	for addr := range l.conns {
		byAge = append(byAge, addr)
	}
	slices.SortFunc(byAge, func(a, b netip.AddrPort) int { return l.conns[a].seen.Compare(l.conns[b].seen) })
	for _, addr := range byAge[:n] {
		delete(l.conns, addr)
	}
}

// prune forgets connections that are quiet and have no TCP connection left.
func (l *Log) prune(now time.Time, live map[netip.AddrPort]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for addr, e := range l.conns {
		if now.Sub(e.seen) > keepUnseen && !live[addr] {
			delete(l.conns, addr)
		}
	}
}
