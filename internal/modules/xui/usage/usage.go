// Package usage counts the traffic of each owned client from the kernel's
// TCP counters, finding each connection's user through the Xray access log.
// Xray adds the bytes of a spliced Vision connection to its user statistics
// only when the connection ends; these counts grow while it runs.
package usage

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
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/sockdiag"
)

const (
	// A connection that closes counts its last bytes from the kernel's notice;
	// polling covers the connections that stay open.
	pollInterval   = 5 * time.Second
	reportInterval = time.Minute
	// A connection missing from this many polls is forgotten. Until then its
	// close notice, which may arrive after the poll, still counts once.
	keepMissing = 6
	maxSafeInt  = 1<<53 - 1
	// A client with no connection and no traffic for idleAfter leaves the
	// report and counts from zero if it comes back.
	idleAfter = 10 * time.Minute
	// More clients than this would not fit one protocol message; the report
	// then says the counts are unavailable rather than send part of them.
	maxClients = 4096
)

// Owners tells whose an inbound connection to Xray is.
type Owners interface {
	Err() error
	Owner(local, remote netip.AddrPort) (string, bool)
}

// Meter polls the kernel, adds what each known connection carried to its
// user, and reports the totals every minute. Only the newest report matters,
// so an unacknowledged one is replaced, never queued.
type Meter struct {
	owners Owners
	dump   func() ([]sockdiag.Conn, error)
	closed func(context.Context, func(sockdiag.Conn)) error
	now    func() time.Time
	wake   chan struct{}

	mu sync.Mutex
	// epoch names the period the totals count from.
	epoch   string
	started bool
	seen    map[uint64]*seen
	clients map[string]*total
	dumpErr error
	pending *protocol.ReportBody
}

var _ module.Reporter = (*Meter)(nil)

// seen is what a connection had carried when it was last counted, and for
// whom.
type seen struct {
	acked, received uint64
	email           string
	missing         int
}

type total struct {
	Email       string `json:"email"`
	Up          uint64 `json:"up"`
	Down        uint64 `json:"down"`
	Connections int    `json:"connections"`
	// active is when the client last had a connection or traffic.
	active time.Time
}

func New(owners Owners) *Meter { return newMeter(owners, sockdiag.TCP, sockdiag.Closed) }

func newMeter(owners Owners, dump func() ([]sockdiag.Conn, error), closed func(context.Context, func(sockdiag.Conn)) error) *Meter {
	return &Meter{owners: owners, dump: dump, closed: closed, now: time.Now, wake: make(chan struct{}, 1),
		epoch: protocol.NewUUID(), seen: map[uint64]*seen{}, clients: map[string]*total{}}
}

func (m *Meter) Run(ctx context.Context, publish module.Publish) error {
	// Without close notices the last seconds of each connection go uncounted;
	// that loss is accepted rather than stopping the meter.
	go func() {
		if err := m.closed(ctx, m.close); err != nil && ctx.Err() == nil {
			slog.Warn("kernel TCP close notices unavailable", "error", err)
		}
	}()
	polls := time.NewTicker(pollInterval)
	defer polls.Stop()
	reports := time.NewTicker(reportInterval)
	defer reports.Stop()
	m.poll()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-polls.C:
			m.poll()
		case <-reports.C:
			m.report(publish)
		case <-m.wake:
			m.report(publish)
		}
	}
}

// poll adds what each open connection carried since it was last counted to
// its user. Connections open before the first poll count from then on; a
// connection whose user is not known yet counts once it is, from then on.
func (m *Meter) poll() {
	conns, err := m.dump()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		if m.dumpErr == nil {
			slog.Warn("kernel TCP counters unavailable", "error", err)
		}
		m.dumpErr = err
		return
	}
	m.dumpErr = nil
	for _, t := range m.clients {
		t.Connections = 0
	}
	for _, s := range m.seen {
		s.missing++
	}
	for _, c := range conns {
		before, found := m.seen[c.Cookie]
		if !found {
			before = &seen{}
			m.seen[c.Cookie] = before
			if !m.started {
				before.acked, before.received = c.BytesAcked, c.BytesReceived
			}
		}
		before.missing = 0
		if t := m.count(c, before, false); t != nil {
			t.Connections++
			t.active = m.now()
		}
	}
	for cookie, s := range m.seen {
		if s.missing > keepMissing {
			delete(m.seen, cookie)
		}
	}
	for email, t := range m.clients {
		if t.Connections == 0 && m.now().Sub(t.active) >= idleAfter {
			delete(m.clients, email)
		}
	}
	m.started = true
}

// close counts what a connection carried after it was last counted. One
// that opened and closed between two polls counts from its first byte.
func (m *Meter) close(c sockdiag.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		return
	}
	before := m.seen[c.Cookie]
	if before == nil {
		before = &seen{}
	}
	m.count(c, before, true)
	// Keep the final count so a later notice or poll adds nothing.
	m.seen[c.Cookie] = before
}

// count adds the bytes since before to the connection's user and moves before
// to now. It returns the user's total, or nil for a connection that is not an
// inbound one of an owned client known from the log; those bytes are skipped.
// A closing connection counts for the user it was counted for while open: by
// the time its notice arrives, another connection may have taken its address.
func (m *Meter) count(c sockdiag.Conn, before *seen, closing bool) *total {
	// Counters only grow; lower ones come from a poll taken before a close
	// notice that was already counted.
	if c.BytesAcked < before.acked || c.BytesReceived < before.received {
		return nil
	}
	acked, received := before.acked, before.received
	before.acked, before.received = c.BytesAcked, c.BytesReceived
	email, ok := before.email, closing && before.email != ""
	if !ok {
		email, ok = m.owners.Owner(c.Local, c.Remote)
	}
	if !ok || !xui.Owned(email) {
		return nil
	}
	before.email = email
	t := m.clients[email]
	if t == nil {
		t = &total{Email: email}
		m.clients[email] = t
	}
	t.Down += c.BytesAcked - acked
	t.Up += c.BytesReceived - received
	if c.BytesAcked > acked || c.BytesReceived > received {
		t.active = m.now()
	}
	// JSON integers stay exact below 2^53; a new epoch starts over.
	if t.Down > maxSafeInt || t.Up > maxSafeInt {
		m.epoch, m.clients = protocol.NewUUID(), map[string]*total{}
		return nil
	}
	return t
}

type report struct {
	Epoch     string   `json:"epoch"`
	ErrorCode string   `json:"error_code"`
	Clients   []*total `json:"clients"`
}

func (m *Meter) report(publish module.Publish) {
	m.mu.Lock()
	r := report{Epoch: m.epoch, Clients: []*total{}}
	switch {
	case m.dumpErr != nil:
		r.ErrorCode = "xui_usage_unavailable"
	case m.owners.Err() != nil:
		r.ErrorCode = "xui_log_unavailable"
	}
	if len(m.clients) > maxClients {
		r.ErrorCode = "xui_usage_unavailable"
	} else {
		for _, email := range slices.Sorted(maps.Keys(m.clients)) {
			r.Clients = append(r.Clients, m.clients[email])
		}
	}
	data, err := json.Marshal(r)
	if err != nil {
		m.mu.Unlock()
		return
	}
	m.pending = &protocol.ReportBody{ReportID: protocol.NewUUID(), Kind: xui.KindUsage, Data: json.RawMessage(data)}
	body := *m.pending
	m.mu.Unlock()
	// Without a session the next report or ready session sends it.
	_ = publish(body)
}

func (m *Meter) ControlReady() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Meter) Acknowledge(reportID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil || m.pending.ReportID != reportID {
		return false, nil
	}
	m.pending = nil
	return true, nil
}

func (m *Meter) UpdateSafe() error { return nil }
