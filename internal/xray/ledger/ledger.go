// Package ledger keeps the one account of the connections to the Xray that
// 3x-ui runs. The kernel says which TCP connections exist and what each has
// carried; the access log says whose each one is. A connection is bound to
// its user when a line names it unambiguously, and stays bound until it
// closes. A line names a connection by inbound port and client address only,
// so two open connections that share both, to two addresses of the node,
// are left unbound: neither counts nor is limited, rather than one being
// counted for the wrong user.
//
// An address holds one TCP connection at a time, so a connection open when
// the Agent started belongs to the newest line of its address: written before
// then, or just after for a connection opened just before; the older lines
// that the log's tail replays are connections already closed, often another
// user's on a port a relay reused.
//
// The ledger tells the xui module how many connections each user holds,
// gives the rate limiter each user's connections and tells whoever keeps
// connection records what a bound connection carried once it ended. A user's
// traffic is Xray's own count, which 3x-ui reports; these per-connection
// bytes say only where it went.
//
// UDP flows have no connection in the kernel's TCP table. A line of an
// inbound that takes UDP, and that no TCP connection took, counts as a UDP
// flow of its user until udpKeep after it was written.
package ledger

import (
	"context"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/sockdiag"
	"github.com/akastrmix/akastr-agent/internal/xray/accesslog"
)

const (
	// The kernel is asked for every connection this often; a closing
	// connection's last bytes come with its close notice.
	dumpInterval = 2 * time.Second
	// A connection missing from this many dumps in a row has closed.
	missingDumps = 2
	// A closed connection is kept this long, so a line read after the close
	// still finds it and counts its bytes.
	keepClosed = 30 * time.Second
	// A line of a TCP-only inbound waits this long for its connection, and a
	// line written before the start this long to take over a connection open
	// then, as lines reach the log a little after they are written.
	claimWait = 10 * time.Second
	udpKeep   = 2 * time.Hour
	// Beyond this many waiting lines, the oldest are dropped.
	maxClaims = 65536
)

// Log is the access log, which names the user of each connection.
type Log interface {
	Run(ctx context.Context, emit func([]accesslog.Line)) error
	Err() error
}

// Ended is a bound connection that closed: the line that bound it, when it
// was found closed, what it carried for its user, and how many lines named a
// request on it, that one included. A multiplexed connection carries many.
type Ended struct {
	Line     accesslog.Line
	At       time.Time
	Up, Down uint64
	Requests int
}

// Flow is one connection or UDP flow of a user, as the rate limiter matches
// it on the way out.
type Flow struct {
	// Proto is "tcp" or "udp".
	Proto string
	// Local is the node's address and port. A UDP flow has the port only:
	// the log does not say which address it reached.
	Local  netip.AddrPort
	Remote netip.AddrPort
}

// Ledger is the account.
type Ledger struct {
	log    Log
	dump   func() ([]sockdiag.Conn, error)
	closed func(context.Context, func(sockdiag.Conn)) error
	now    func() time.Time
	// ended is told of each bound connection once it is gone for good.
	ended func(Ended)

	mu sync.Mutex
	// dumped is when the last successful dump started, zero before the first;
	// started is when the first one did; tail is when the first lines read
	// after it came in, the log's tail, however late the log began.
	dumped, started, tail time.Time
	dumpErr               error
	sockets               map[uint64]*socket
	byConn                map[accesslog.Conn][]*socket
	// claims are the newest line of each connection the log named that is
	// still waiting for its TCP connection or counts as a UDP flow.
	claims map[accesslog.Conn]*claim
	// finished are bound connections removed since ended was last told.
	finished []Ended
}

var _ module.Worker = (*Ledger)(nil)

// socket is one TCP connection of the host.
type socket struct {
	cookie uint64
	conn   accesslog.Conn
	local  netip.AddrPort
	// email is the bound user, empty until then, and line the line that
	// bound it; requests counts the lines that named it.
	email    string
	line     accesslog.Line
	requests int
	// The kernel's counters: all the connection carried, down and up.
	acked, received uint64
	// after is a time the connection opened after: the start of the dump
	// before it was first seen. Zero for one open at the first dump.
	after time.Time
	// closedAt is when the connection was found closed: by the kernel's
	// notice, which is final, or by missing from dumps, which a later dump
	// that shows it again undoes.
	closedAt time.Time
	noticed  bool
	missing  int
}

type claim struct {
	line accesslog.Line
	// arrived is when the ledger got the line; requests counts it and the
	// lines of its user it replaced while waiting, one connection's requests.
	arrived  time.Time
	requests int
}

// Connections are each user's open bound TCP connections, and why there may
// be more than they say.
type Connections struct {
	// Kernel is why the kernel's connections cannot be read, which stops the
	// counts; Log is why the access log cannot, so new connections go unfound.
	Kernel, Log error
	Users       map[string]int
}

// New keeps the account of the connections log names; ended, when not nil,
// is told of each bound connection once it is gone and must return quickly.
func New(log Log, ended func(Ended)) *Ledger {
	l := newLedger(log, sockdiag.TCP, sockdiag.Closed)
	l.ended = ended
	return l
}

func newLedger(log Log, dump func() ([]sockdiag.Conn, error), closed func(context.Context, func(sockdiag.Conn)) error) *Ledger {
	return &Ledger{log: log, dump: dump, closed: closed, now: time.Now,
		sockets: map[uint64]*socket{}, byConn: map[accesslog.Conn][]*socket{}, claims: map[accesslog.Conn]*claim{}}
}

// Run keeps the account until ctx ends.
func (l *Ledger) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Without close notices a connection's bytes after its last dump go
		// uncounted; that loss is accepted rather than stopping the ledger.
		if err := l.closed(ctx, l.close); err != nil && ctx.Err() == nil {
			slog.Warn("kernel TCP close notices unavailable", "error", err)
		}
	}()
	// The first dump comes before the log, so that the lines already in the
	// log find the connections open before the Agent started.
	l.poll()
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = l.log.Run(ctx, l.claim)
	}()
	polls := time.NewTicker(dumpInterval)
	defer polls.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-polls.C:
			l.poll()
		}
	}
}

// poll takes in a dump of the kernel's TCP connections, then tells of the
// bound connections it found gone.
func (l *Ledger) poll() {
	start := l.now()
	conns, err := l.dump()
	l.mu.Lock()
	l.take(start, conns, err)
	finished := l.finished
	l.finished = nil
	l.mu.Unlock()
	for _, e := range finished {
		l.ended(e)
	}
}

// take updates the account from one dump; the caller holds the lock.
func (l *Ledger) take(start time.Time, conns []sockdiag.Conn, err error) {
	now := l.now()
	if err != nil {
		if l.dumpErr == nil {
			slog.Warn("kernel TCP counters unavailable", "error", err)
		}
		l.dumpErr = err
		l.expire(now)
		return
	}
	l.dumpErr = nil
	for _, s := range l.sockets {
		if s.closedAt.IsZero() {
			s.missing++
		}
	}
	var added []*socket
	for _, c := range conns {
		s := l.sockets[c.Cookie]
		switch {
		case s == nil:
			added = append(added, l.add(c))
		case !s.noticed:
			// A dump taken before a close notice and read after it shows a
			// connection that is gone; any other dump that shows it is right.
			s.missing, s.closedAt = 0, time.Time{}
			l.update(s, c)
		}
	}
	// Only with the whole dump in can a line tell whether it names one
	// connection or two.
	for _, s := range added {
		l.bindSocket(s)
	}
	for _, s := range l.sockets {
		if s.closedAt.IsZero() && s.missing >= missingDumps {
			s.closedAt = now
		}
	}
	if l.dumped.IsZero() {
		l.started = start
	}
	l.dumped = start
	l.expire(now)
}

// close takes in the final counters of a connection the kernel destroyed.
func (l *Ledger) close(c sockdiag.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dumped.IsZero() {
		return
	}
	now := l.now()
	s := l.sockets[c.Cookie]
	if s == nil {
		// Opened and closed between two dumps.
		s = l.add(c)
		s.closedAt, s.noticed = now, true
		l.bindSocket(s)
		return
	}
	l.update(s, c)
	if s.closedAt.IsZero() {
		s.closedAt = now
	}
	s.noticed = true
}

// claim takes in the access log's lines, in the order they were written.
func (l *Ledger) claim(batch []accesslog.Line) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.tail.IsZero() && !l.started.IsZero() {
		l.tail = now
	}
	for _, line := range batch {
		c := &claim{line: line, arrived: now, requests: 1}
		if s := l.replayed(line, now); s != nil {
			s.email, s.line, s.requests = line.Email, line, 1
			delete(l.claims, line.Conn)
			continue
		}
		// A line a TCP connection takes ends what waited at its address.
		if s := l.carrier(line.Conn, c); s != nil {
			// Another line of a connection already bound, as a multiplexed
			// connection writes for each request: not a UDP flow.
			s.requests++
			delete(l.claims, line.Conn)
			continue
		}
		if s := l.candidate(line.Conn, c); s != nil {
			l.bind(s, c)
			delete(l.claims, line.Conn)
			continue
		}
		// The newest line of a connection is the one that waits; the user's
		// earlier ones are requests on the same connection.
		if waiting := l.claims[line.Conn]; waiting != nil && waiting.line.Email == line.Email {
			c.requests += waiting.requests
		}
		l.claims[line.Conn] = c
	}
	if len(l.claims) > maxClaims {
		l.dropOldClaims()
	}
}

func (l *Ledger) add(c sockdiag.Conn) *socket {
	s := &socket{cookie: c.Cookie, conn: accesslog.Conn{Port: c.Local.Port(), Remote: c.Remote}, local: c.Local,
		acked: c.BytesAcked, received: c.BytesReceived, after: l.dumped}
	l.sockets[c.Cookie] = s
	l.byConn[s.conn] = append(l.byConn[s.conn], s)
	return s
}

func (l *Ledger) remove(s *socket) {
	if s.email != "" && l.ended != nil {
		l.finished = append(l.finished, Ended{Line: s.line, At: s.closedAt, Up: s.received, Down: s.acked, Requests: s.requests})
	}
	delete(l.sockets, s.cookie)
	rest := slices.DeleteFunc(l.byConn[s.conn], func(o *socket) bool { return o == s })
	if len(rest) == 0 {
		delete(l.byConn, s.conn)
	} else {
		l.byConn[s.conn] = rest
	}
}

// update moves a connection's counters forward. Counters only grow; lower
// ones come from a dump taken before a close notice and are skipped.
func (l *Ledger) update(s *socket, c sockdiag.Conn) {
	if c.BytesAcked < s.acked || c.BytesReceived < s.received {
		return
	}
	s.acked, s.received = c.BytesAcked, c.BytesReceived
}

// bindSocket binds a connection the kernel just showed when the waiting line
// of its address is its own.
func (l *Ledger) bindSocket(s *socket) {
	if c := l.claims[s.conn]; c != nil && l.candidate(s.conn, c) == s {
		l.bind(s, c)
		delete(l.claims, s.conn)
	}
}

// carrier is a connection at conn bound to the line's user that was open
// when the line was written, or nil. A line read after the connection closed
// may still be its own; one written after the close is not.
func (l *Ledger) carrier(conn accesslog.Conn, c *claim) *socket {
	for _, s := range l.byConn[conn] {
		if s.email == c.line.Email && (s.closedAt.IsZero() || !c.line.At.After(s.closedAt)) {
			return s
		}
	}
	return nil
}

// candidate is the one unbound connection a line can belong to: one that
// opened before the line was written and had not closed by then. With none
// or several, the line binds nothing.
func (l *Ledger) candidate(conn accesslog.Conn, c *claim) *socket {
	var found *socket
	for _, s := range l.byConn[conn] {
		if s.email != "" || c.line.At.Before(s.after) || (!s.closedAt.IsZero() && c.line.At.After(s.closedAt)) {
			continue
		}
		if found != nil {
			return nil
		}
		found = s
	}
	return found
}

// bind gives a connection its user.
func (l *Ledger) bind(s *socket, c *claim) {
	s.email, s.line, s.requests = c.line.Email, c.line, c.requests
}

// replayed is a connection open at the start, bound to an older line, that a
// newer line takes over while the log's tail is still coming in. An address
// holds one TCP connection at a time, so the newest line of an open
// connection's address is its own: one written before the start, or one written
// just after it for a connection opened just before, whose line came late.
// Older lines are dropped, not counted as its requests: they may be closed
// connections of the same user. A line written since the start takes over only
// for another user, as the same user's is a request on the connection, and only
// when the connection is the one at its address and was open then.
func (l *Ledger) replayed(line accesslog.Line, now time.Time) *socket {
	if line.UDP || l.started.IsZero() || now.Sub(l.tail) > claimWait {
		return nil
	}
	late := line.At.After(l.started)
	sockets := l.byConn[line.Conn]
	for _, s := range sockets {
		if !s.after.IsZero() || s.email == "" || !line.At.After(s.line.At) {
			continue
		}
		if late && (len(sockets) > 1 || line.Email == s.email || (!s.closedAt.IsZero() && line.At.After(s.closedAt))) {
			return nil
		}
		return s
	}
	return nil
}

// expire forgets closed connections and lines whose time is over.
func (l *Ledger) expire(now time.Time) {
	for _, s := range l.sockets {
		if !s.closedAt.IsZero() && now.Sub(s.closedAt) > keepClosed {
			l.remove(s)
		}
	}
	for conn, c := range l.claims {
		if (c.line.UDP && now.Sub(c.line.At) > udpKeep) || (!c.line.UDP && now.Sub(c.arrived) > claimWait) {
			delete(l.claims, conn)
		}
	}
}

func (l *Ledger) dropOldClaims() {
	conns := slices.Collect(maps.Keys(l.claims))
	slices.SortFunc(conns, func(a, b accesslog.Conn) int { return l.claims[a].arrived.Compare(l.claims[b].arrived) })
	for _, conn := range conns[:len(conns)-maxClaims*9/10] {
		delete(l.claims, conn)
	}
}

// Flows returns the open connections and live UDP flows of each named user.
func (l *Ledger) Flows(emails map[string]bool) map[string][]Flow {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	out := map[string][]Flow{}
	for _, s := range l.sockets {
		if emails[s.email] && s.closedAt.IsZero() {
			out[s.email] = append(out[s.email], Flow{Proto: "tcp", Local: s.local, Remote: s.conn.Remote})
		}
	}
	for conn, c := range l.claims {
		if c.line.UDP && emails[c.line.Email] && now.Sub(c.line.At) <= udpKeep {
			out[c.line.Email] = append(out[c.line.Email], Flow{Proto: "udp", Local: netip.AddrPortFrom(netip.Addr{}, conn.Port), Remote: conn.Remote})
		}
	}
	return out
}

// Err reports why connections cannot all be found, or nil.
func (l *Ledger) Err() error {
	if err := l.log.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dumpErr
}

// Connections counts each user's open bound connections.
func (l *Ledger) Connections() Connections {
	logErr := l.log.Err()
	l.mu.Lock()
	defer l.mu.Unlock()
	c := Connections{Kernel: l.dumpErr, Log: logErr, Users: map[string]int{}}
	for _, s := range l.sockets {
		if s.email != "" && s.closedAt.IsZero() {
			c.Users[s.email]++
		}
	}
	return c
}
