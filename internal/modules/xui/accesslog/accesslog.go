// Package accesslog follows the access log of the Xray that 3x-ui runs and
// keeps, in memory only, which user each connection belongs to. A connection
// is known by the inbound port it arrived on and the client address and port
// Xray accepted it from, which is also where its download goes; behind a
// relay that is the relay's address, one per relayed connection. One relay
// address may reach two inbounds at once, so the port is part of the key.
// Nothing here leaves the process.
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

	"github.com/akastrmix/akastr-agent/internal/sockdiag"
)

const (
	pollInterval  = time.Second
	locateEvery   = 5 * time.Minute
	locateRetry   = 10 * time.Second
	pruneInterval = time.Minute
	// A closed TCP connection is forgotten after this grace, which leaves its
	// close notice time to be counted. A connection that may be UDP has no
	// socket to look for, so it is forgotten once no line named it for
	// keepUnseen.
	keepClosed = time.Minute
	keepUnseen = 2 * time.Hour
	maxEntries = 65536
	// persistentCopy is where 3x-ui appends the log before truncating it.
	persistentCopy = "/var/log/x-ui/3xipl-ap.log"
)

// Conn is one connection Xray accepted: the inbound port it arrived on and
// the client address it came from.
type Conn struct {
	Port   uint16
	Remote netip.AddrPort
}

// Log is the connection table and the loop that keeps it current.
type Log struct {
	proc, copy string
	// live lists the open TCP connections of the host.
	live func() (map[Conn]bool, error)

	mu    sync.RWMutex
	conns map[Conn]entry
	// inbounds are the running Xray configuration's inbounds by tag; udp
	// tells, by port, which may carry UDP.
	inbounds map[string]inbound
	udp      map[uint16]bool
	err      error
}

type entry struct {
	email string
	seen  time.Time
	// tcp is set once the connection was found open as TCP.
	tcp bool
}

func New() *Log { return newLog("/proc", liveTCP) }

func newLog(proc string, live func() (map[Conn]bool, error)) *Log {
	return &Log{proc: proc, copy: persistentCopy, live: live, conns: map[Conn]entry{},
		inbounds: map[string]inbound{}, udp: map[uint16]bool{}, err: errors.New("xray access log not read yet")}
}

// liveTCP asks the kernel for the open TCP connections.
func liveTCP() (map[Conn]bool, error) {
	conns, err := sockdiag.TCP()
	if err != nil {
		return nil, err
	}
	live := make(map[Conn]bool, len(conns))
	for _, c := range conns {
		live[Conn{c.Local.Port(), c.Remote}] = true
	}
	return live, nil
}

// Err reports why the log is not being followed, or nil while it is.
func (l *Log) Err() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.err
}

// Connections returns the known connections of each named email.
func (l *Log) Connections(emails map[string]bool) map[string][]Conn {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := map[string][]Conn{}
	for c, e := range l.conns {
		if emails[e.email] {
			out[e.email] = append(out[e.email], c)
		}
	}
	return out
}

// Owner returns the user of the connection from remote to the local address.
func (l *Log) Owner(local, remote netip.AddrPort) (string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	e, found := l.conns[Conn{local.Port(), remote}]
	return e.email, found
}

// Run follows the log until ctx ends. Losing the log is not fatal: the table
// keeps its connections and Err says why, until the log is found again.
func (l *Log) Run(ctx context.Context) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	var f *follower
	defer func() { f.close() }()
	var locateAt, pruneAt time.Time
	seeded := false
	for {
		now := time.Now()
		if !now.Before(locateAt) {
			path, inbounds, err := locate(l.proc)
			if err == nil {
				l.setInbounds(inbounds)
				if !seeded {
					l.seed(now)
					seeded = true
				}
			}
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
				if tag, addr, email, ok := parse(raw); ok {
					lines = append(lines, line{tag, addr, email})
				}
			})
			l.add(lines, now, nil)
			l.setErr(err)
			if err != nil {
				f.close()
				f, locateAt = nil, now.Add(locateRetry)
			}
		}
		if !now.Before(pruneAt) {
			if live, err := l.live(); err == nil {
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
	live, err := l.live()
	if err != nil {
		return
	}
	f := &follower{path: l.copy}
	defer f.close()
	var lines []line
	_ = f.poll(func(raw []byte) {
		if tag, addr, email, ok := parse(raw); ok {
			lines = append(lines, line{tag, addr, email})
		}
	})
	l.add(lines, now, live)
}

type line struct {
	tag   string
	addr  netip.AddrPort
	email string
}

func (l *Log) setErr(err error) {
	l.mu.Lock()
	l.err = err
	l.mu.Unlock()
}

func (l *Log) setInbounds(inbounds map[string]inbound) {
	udp := map[uint16]bool{}
	for _, ib := range inbounds {
		udp[ib.port] = udp[ib.port] || ib.udp
	}
	l.mu.Lock()
	l.inbounds, l.udp = inbounds, udp
	l.mu.Unlock()
}

// add records lines in order, so a reused connection belongs to its newest
// user. Lines of inbounds the configuration does not name are skipped, and so
// are those not in only, when it is given.
func (l *Log) add(lines []line, now time.Time, only map[Conn]bool) {
	if len(lines) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ln := range lines {
		ib, found := l.inbounds[ln.tag]
		c := Conn{ib.port, ln.addr}
		if !found || (only != nil && !only[c]) {
			continue
		}
		l.conns[c] = entry{email: ln.email, seen: now, tcp: only != nil}
	}
	if len(l.conns) > maxEntries {
		l.evictOldest(len(l.conns) - maxEntries*9/10)
	}
}

// evictOldest drops the n least recently seen connections; l.mu must be held.
func (l *Log) evictOldest(n int) {
	byAge := make([]Conn, 0, len(l.conns))
	for c := range l.conns {
		byAge = append(byAge, c)
	}
	slices.SortFunc(byAge, func(a, b Conn) int { return l.conns[a].seen.Compare(l.conns[b].seen) })
	for _, c := range byAge[:n] {
		delete(l.conns, c)
	}
}

// prune forgets connections that ended: a TCP connection soon after it
// closed, and one that may be UDP once it has been quiet for keepUnseen.
func (l *Log) prune(now time.Time, live map[Conn]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for c, e := range l.conns {
		switch {
		case live[c]:
			e.tcp, e.seen = true, now
			l.conns[c] = e
		case e.tcp || !l.udp[c.Port]:
			if now.Sub(e.seen) > keepClosed {
				delete(l.conns, c)
			}
		case now.Sub(e.seen) > keepUnseen:
			delete(l.conns, c)
		}
	}
}
