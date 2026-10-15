// Package ledger keeps the one account of the connections to the Xray that
// 3x-ui runs. The kernel says which TCP connections exist and what each has
// carried; the access log says whose each one is. A connection is bound to
// its user when a line names it unambiguously, and stays bound until it
// closes. A line names a connection by inbound port and client address only,
// so two open connections that share both, to two addresses of the node,
// are left unbound: neither counts nor is limited, rather than one being
// counted for the wrong user.
//
// The ledger reports each owned client's traffic to Cloud as xui.usage and
// gives the rate limiter each user's connections. Xray adds the bytes of a
// spliced Vision connection to its user statistics only when the connection
// ends; these counts grow while it runs.
//
// UDP flows have no connection in the kernel's TCP table. A line of an
// inbound that takes UDP, and that no TCP connection took, counts as a UDP
// flow of its user until udpKeep after it was written.
package ledger

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/modules/xui"
	"github.com/akastrmix/akastr-agent/internal/modules/xui/accesslog"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/sockdiag"
)

const (
	// The kernel is asked for every connection this often; a closing
	// connection's last bytes come with its close notice.
	dumpInterval   = 2 * time.Second
	reportInterval = time.Minute
	// A connection missing from this many dumps in a row has closed.
	missingDumps = 2
	// A closed connection is kept this long, so a line read after the close
	// still finds it and counts its bytes.
	keepClosed = 30 * time.Second
	// A line of a TCP-only inbound waits this long for its connection.
	claimWait = 10 * time.Second
	// A connection found more than lateBind after it was first seen counts
	// from then on: a late line must not add a lump of earlier traffic.
	lateBind = 30 * time.Second
	udpKeep  = 2 * time.Hour
	// Beyond this many waiting lines, the oldest are dropped.
	maxClaims  = 65536
	maxSafeInt = 1<<53 - 1
	// A client with no connection and no traffic for idleAfter leaves the
	// report and counts from zero if it comes back.
	idleAfter = 10 * time.Minute
	// More clients than this would not fit one protocol message; the report
	// then says the counts are unavailable rather than send part of them.
	maxClients = 4096
)

// Log is the access log, which names the user of each connection.
type Log interface {
	Run(ctx context.Context, emit func([]accesslog.Claim)) error
	Err() error
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

// Ledger is the account. Only the newest usage report matters, so an
// unacknowledged one is replaced, never queued.
type Ledger struct {
	log    Log
	dump   func() ([]sockdiag.Conn, error)
	closed func(context.Context, func(sockdiag.Conn)) error
	now    func() time.Time
	wake   chan struct{}

	mu sync.Mutex
	// dumped is when the last successful dump started, zero before the first.
	dumped  time.Time
	dumpErr error
	sockets map[uint64]*socket
	byConn  map[accesslog.Conn][]*socket
	// claims are the newest line of each connection the log named that is
	// still waiting for its TCP connection or counts as a UDP flow.
	claims map[accesslog.Conn]*claim
	// epoch names the period the totals count from.
	epoch   string
	clients map[string]*total
	pending *protocol.ReportBody
}

var _ module.Reporter = (*Ledger)(nil)

// socket is one TCP connection of the host.
type socket struct {
	cookie uint64
	conn   accesslog.Conn
	local  netip.AddrPort
	// email is the bound user, empty until then.
	email string
	// The counters when first seen, the latest, and what the user was given.
	firstAcked, firstReceived     uint64
	acked, received               uint64
	countedAcked, countedReceived uint64
	// after is a time the connection opened after: the start of the dump
	// before it was first seen. Zero for one open at the first dump.
	after     time.Time
	firstSeen time.Time
	// closedAt is when the connection was found closed: by the kernel's
	// notice, which is final, or by missing from dumps, which a later dump
	// that shows it again undoes.
	closedAt time.Time
	noticed  bool
	missing  int
}

type claim struct {
	email string
	// at is when the line was written, arrived when the ledger got it.
	at, arrived time.Time
	udp         bool
}

type total struct {
	Email       string `json:"email"`
	Up          uint64 `json:"up"`
	Down        uint64 `json:"down"`
	Connections int    `json:"connections"`
	// active is when the client last had a connection or traffic.
	active time.Time
}

func New(log Log) *Ledger { return newLedger(log, sockdiag.TCP, sockdiag.Closed) }

func newLedger(log Log, dump func() ([]sockdiag.Conn, error), closed func(context.Context, func(sockdiag.Conn)) error) *Ledger {
	return &Ledger{log: log, dump: dump, closed: closed, now: time.Now, wake: make(chan struct{}, 1),
		sockets: map[uint64]*socket{}, byConn: map[accesslog.Conn][]*socket{}, claims: map[accesslog.Conn]*claim{},
		epoch: protocol.NewUUID(), clients: map[string]*total{}}
}

// Run keeps the account and reports it every minute until ctx ends.
func (l *Ledger) Run(ctx context.Context, publish module.Publish) error {
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
	reports := time.NewTicker(reportInterval)
	defer reports.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-polls.C:
			l.poll()
		case <-reports.C:
			l.report(publish)
		case <-l.wake:
			l.report(publish)
		}
	}
}

// poll takes in a dump of the kernel's TCP connections.
func (l *Ledger) poll() {
	start := l.now()
	conns, err := l.dump()
	l.mu.Lock()
	defer l.mu.Unlock()
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
			added = append(added, l.add(c, now))
		case !s.noticed:
			// A dump taken before a close notice and read after it shows a
			// connection that is gone; any other dump that shows it is right.
			s.missing, s.closedAt = 0, time.Time{}
			l.update(s, c, now)
		}
	}
	// Only with the whole dump in can a line tell whether it names one
	// connection or two.
	for _, s := range added {
		l.bindSocket(s, now)
	}
	for _, s := range l.sockets {
		if s.closedAt.IsZero() && s.missing >= missingDumps {
			s.closedAt = now
		}
	}
	l.dumped = start
	l.expire(now)
	l.tally(now)
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
		s = l.add(c, now)
		s.closedAt, s.noticed = now, true
		l.bindSocket(s, now)
		return
	}
	l.update(s, c, now)
	if s.closedAt.IsZero() {
		s.closedAt = now
	}
	s.noticed = true
}

// claim takes in the access log's lines, in the order they were written.
func (l *Ledger) claim(batch []accesslog.Claim) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	// The newest line of a connection is the one that counts.
	touched := make([]accesslog.Conn, 0, len(batch))
	for _, c := range batch {
		l.claims[c.Conn] = &claim{email: c.Email, at: c.At, arrived: now, udp: c.UDP}
		touched = append(touched, c.Conn)
	}
	for _, conn := range touched {
		c := l.claims[conn]
		switch {
		case c == nil:
		case l.carries(conn, c):
			// Another line of a connection already bound, as a multiplexed
			// connection writes for each request: not a UDP flow.
			delete(l.claims, conn)
		default:
			if s := l.candidate(conn, c); s != nil {
				l.bind(s, c, now)
				delete(l.claims, conn)
			}
		}
	}
	if len(l.claims) > maxClaims {
		l.dropOldClaims()
	}
}

func (l *Ledger) add(c sockdiag.Conn, now time.Time) *socket {
	s := &socket{cookie: c.Cookie, conn: accesslog.Conn{Port: c.Local.Port(), Remote: c.Remote}, local: c.Local,
		firstAcked: c.BytesAcked, firstReceived: c.BytesReceived, acked: c.BytesAcked, received: c.BytesReceived,
		after: l.dumped, firstSeen: now}
	l.sockets[c.Cookie] = s
	l.byConn[s.conn] = append(l.byConn[s.conn], s)
	return s
}

func (l *Ledger) remove(s *socket) {
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
func (l *Ledger) update(s *socket, c sockdiag.Conn, now time.Time) {
	if c.BytesAcked < s.acked || c.BytesReceived < s.received {
		return
	}
	s.acked, s.received = c.BytesAcked, c.BytesReceived
	if s.email != "" {
		l.credit(s, now)
	}
}

// bindSocket binds a connection the kernel just showed when the waiting line
// of its address is its own.
func (l *Ledger) bindSocket(s *socket, now time.Time) {
	if c := l.claims[s.conn]; c != nil && l.candidate(s.conn, c) == s {
		l.bind(s, c, now)
		delete(l.claims, s.conn)
	}
}

// carries reports whether a connection at conn bound to the line's user was
// open when the line was written. A line read after the connection closed
// may still be its own; one written after the close is not.
func (l *Ledger) carries(conn accesslog.Conn, c *claim) bool {
	for _, s := range l.byConn[conn] {
		if s.email == c.email && (s.closedAt.IsZero() || !c.at.After(s.closedAt)) {
			return true
		}
	}
	return false
}

// candidate is the one unbound connection a line can belong to: one that
// opened before the line was written and had not closed by then. With none
// or several, the line binds nothing.
func (l *Ledger) candidate(conn accesslog.Conn, c *claim) *socket {
	var found *socket
	for _, s := range l.byConn[conn] {
		if s.email != "" || c.at.Before(s.after) || (!s.closedAt.IsZero() && c.at.After(s.closedAt)) {
			continue
		}
		if found != nil {
			return nil
		}
		found = s
	}
	return found
}

// bind gives a connection its user. It counts from its first byte when found
// soon after it was first seen; a connection open before the first dump
// counts from that dump, and one found late from now.
func (l *Ledger) bind(s *socket, c *claim, now time.Time) {
	s.email = c.email
	switch {
	case now.Sub(s.firstSeen) > lateBind:
		s.countedAcked, s.countedReceived = s.acked, s.received
	case s.after.IsZero():
		s.countedAcked, s.countedReceived = s.firstAcked, s.firstReceived
	}
	l.credit(s, now)
}

// credit adds what a bound connection carried since it was last counted to
// its user, when the user is an owned client.
func (l *Ledger) credit(s *socket, now time.Time) {
	down, up := s.acked-s.countedAcked, s.received-s.countedReceived
	s.countedAcked, s.countedReceived = s.acked, s.received
	if !xui.Owned(s.email) {
		return
	}
	t := l.clients[s.email]
	if t == nil {
		t = &total{Email: s.email, active: now}
		l.clients[s.email] = t
	}
	if down == 0 && up == 0 {
		return
	}
	t.Down += down
	t.Up += up
	t.active = now
	// JSON integers stay exact below 2^53; a new epoch starts over.
	if t.Down > maxSafeInt || t.Up > maxSafeInt {
		l.epoch, l.clients = protocol.NewUUID(), map[string]*total{}
	}
}

// expire forgets closed connections and lines whose time is over.
func (l *Ledger) expire(now time.Time) {
	for _, s := range l.sockets {
		if !s.closedAt.IsZero() && now.Sub(s.closedAt) > keepClosed {
			l.remove(s)
		}
	}
	for conn, c := range l.claims {
		if (c.udp && now.Sub(c.at) > udpKeep) || (!c.udp && now.Sub(c.arrived) > claimWait) {
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

// tally counts each client's open connections.
func (l *Ledger) tally(now time.Time) {
	for _, t := range l.clients {
		t.Connections = 0
	}
	for _, s := range l.sockets {
		if t := l.clients[s.email]; t != nil && s.closedAt.IsZero() {
			t.Connections++
			t.active = now
		}
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
		if c.udp && emails[c.email] && now.Sub(c.at) <= udpKeep {
			out[c.email] = append(out[c.email], Flow{Proto: "udp", Local: netip.AddrPortFrom(netip.Addr{}, conn.Port), Remote: conn.Remote})
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

type report struct {
	Epoch     string   `json:"epoch"`
	ErrorCode string   `json:"error_code"`
	Clients   []*total `json:"clients"`
}

func (l *Ledger) report(publish module.Publish) {
	logErr := l.log.Err()
	l.mu.Lock()
	now := l.now()
	l.tally(now)
	// Idle clients leave only here, so that the report saying so comes before
	// any report in which the client starts again from zero.
	for email, t := range l.clients {
		if t.Connections == 0 && now.Sub(t.active) >= idleAfter {
			delete(l.clients, email)
		}
	}
	r := report{Epoch: l.epoch, Clients: []*total{}}
	switch {
	case l.dumpErr != nil:
		r.ErrorCode = "xui_usage_unavailable"
	case logErr != nil:
		r.ErrorCode = "xui_log_unavailable"
	}
	if len(l.clients) > maxClients {
		r.ErrorCode = "xui_usage_unavailable"
	} else {
		for _, email := range slices.Sorted(maps.Keys(l.clients)) {
			r.Clients = append(r.Clients, l.clients[email])
		}
	}
	data, err := json.Marshal(r)
	if err != nil {
		l.mu.Unlock()
		return
	}
	l.pending = &protocol.ReportBody{ReportID: protocol.NewUUID(), Kind: xui.KindUsage, Data: json.RawMessage(data)}
	body := *l.pending
	l.mu.Unlock()
	// Without a session the next report or ready session sends it.
	_ = publish(body)
}

func (l *Ledger) ControlReady() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *Ledger) Acknowledge(reportID string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending == nil || l.pending.ReportID != reportID {
		return false, nil
	}
	l.pending = nil
	return true, nil
}

func (l *Ledger) UpdateSafe() error { return nil }
