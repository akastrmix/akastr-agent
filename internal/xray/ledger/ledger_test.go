package ledger

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/sockdiag"
	"github.com/akastrmix/akastr-agent/internal/xray/accesslog"
)

var (
	nodeA = netip.MustParseAddr("198.51.100.1")
	nodeB = netip.MustParseAddr("198.51.100.2")
)

// harness is a kernel, an access log and a clock the test drives by hand.
type harness struct {
	t       *testing.T
	l       *Ledger
	kernel  map[uint64]sockdiag.Conn
	dumpErr error
	logErr  error
	now     time.Time
	ended   []Ended
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, kernel: map[uint64]sockdiag.Conn{}, now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	h.l = newLedger(h, h.dump, func(context.Context, func(sockdiag.Conn)) error { return nil })
	h.l.now = func() time.Time { return h.now }
	h.l.ended = func(e Ended) { h.ended = append(h.ended, e) }
	return h
}

func (h *harness) Run(ctx context.Context, _ func([]accesslog.Line)) error {
	<-ctx.Done()
	return ctx.Err()
}

func (h *harness) Err() error { return h.logErr }

func (h *harness) dump() ([]sockdiag.Conn, error) {
	return slices.Collect(maps.Values(h.kernel)), h.dumpErr
}

// open adds a connection from remote to port on local that already carried
// down bytes to the client.
func (h *harness) open(cookie uint64, local netip.Addr, port uint16, remote string, down uint64) {
	h.kernel[cookie] = sockdiag.Conn{Cookie: cookie, Local: netip.AddrPortFrom(local, port), Remote: netip.MustParseAddrPort(remote), BytesAcked: down}
}

func (h *harness) send(cookie uint64, down, up uint64) {
	c := h.kernel[cookie]
	c.BytesAcked += down
	c.BytesReceived += up
	h.kernel[cookie] = c
}

// shut closes a connection after it carried down more bytes, with or
// without the kernel's notice.
func (h *harness) shut(cookie uint64, down uint64, notice bool) {
	h.send(cookie, down, 0)
	c := h.kernel[cookie]
	delete(h.kernel, cookie)
	if notice {
		h.l.close(c)
	}
}

// tick lets d pass and dumps.
func (h *harness) tick(d time.Duration) {
	h.now = h.now.Add(d)
	h.l.poll()
}

// line passes a line written at at about remote on port.
func (h *harness) line(at time.Time, port uint16, remote, email string, udp bool) {
	h.l.claim([]accesslog.Line{lineAt(at, port, remote, email, udp)})
}

func lineAt(at time.Time, port uint16, remote, email string, udp bool) accesslog.Line {
	return accesslog.Line{Conn: accesslog.Conn{Port: port, Remote: netip.MustParseAddrPort(remote)}, Email: email, At: at, Written: at, UDP: udp}
}

// totals are each user's down bytes and connections.
func (h *harness) totals() map[string][2]uint64 {
	h.t.Helper()
	out := map[string][2]uint64{}
	for _, u := range h.l.Usage().Users {
		out[u.Email] = [2]uint64{u.Down, uint64(u.Connections)}
	}
	return out
}

func (h *harness) flows(email string) []string {
	var out []string
	for _, f := range h.l.Flows(map[string]bool{email: true})[email] {
		local := f.Local.String()
		if !f.Local.Addr().IsValid() {
			local = fmt.Sprintf(":%d", f.Local.Port())
		}
		out = append(out, f.Proto+" "+local+" "+f.Remote.String())
	}
	slices.Sort(out)
	return out
}

func (h *harness) expect(want map[string][2]uint64) {
	h.t.Helper()
	if got := h.totals(); !reflect.DeepEqual(got, want) {
		h.t.Fatalf("totals %v, want %v", got, want)
	}
}

// A connection is found whether the kernel or the log shows it first, and a
// short one whose line is read after it closed still counts in full.
func TestBindsWhicheverComesFirst(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	// The kernel first.
	h.open(1, nodeA, 443, "203.0.113.9:1001", 0)
	h.tick(2 * time.Second)
	h.send(1, 1000, 10)
	h.line(h.now, 443, "203.0.113.9:1001", "ak-a", false)
	// The log first; the connection carried bytes before the next dump.
	h.line(h.now, 443, "203.0.113.9:1002", "ak-b", false)
	h.open(2, nodeA, 443, "203.0.113.9:1002", 2000)
	h.tick(2 * time.Second)
	// Opened and closed between two dumps, its line read after the close.
	written := h.now
	h.open(3, nodeA, 443, "203.0.113.9:1003", 0)
	h.now = h.now.Add(time.Second)
	h.shut(3, 3000, true)
	h.line(written, 443, "203.0.113.9:1003", "ak-c", false)
	h.tick(time.Second)
	h.expect(map[string][2]uint64{"ak-a": {1000, 1}, "ak-b": {2000, 1}, "ak-c": {3000, 0}})
	if got := h.flows("ak-a"); !reflect.DeepEqual(got, []string{"tcp 198.51.100.1:443 203.0.113.9:1001"}) {
		t.Fatalf("flows %v", got)
	}
	// A closed connection leaves the limiter at once; its last bytes count.
	h.shut(1, 500, true)
	if got := h.flows("ak-a"); len(got) != 0 {
		t.Fatalf("closed connection still limited: %v", got)
	}
	// Without a notice, it is gone after two dumps, unless a dump shows it
	// again.
	missed := h.kernel[2]
	h.shut(2, 0, false)
	h.tick(2 * time.Second)
	if len(h.flows("ak-b")) != 1 {
		t.Fatal("one missed dump closed a connection")
	}
	h.tick(2 * time.Second)
	if len(h.flows("ak-b")) != 0 {
		t.Fatal("a connection missing from two dumps is still limited")
	}
	h.kernel[2] = missed
	h.tick(2 * time.Second)
	if len(h.flows("ak-b")) != 1 {
		t.Fatal("a connection shown again stays closed")
	}
	h.shut(2, 0, false)
	h.tick(2 * time.Second)
	h.tick(2 * time.Second)
	h.expect(map[string][2]uint64{"ak-a": {1500, 0}, "ak-b": {2000, 0}, "ak-c": {3000, 0}})
	if len(h.flows("ak-b")) != 0 {
		t.Fatal("a connection missing from two dumps is still limited")
	}
}

// Traffic from before the Agent started, or before a late line, never comes
// as one lump: it would look like a heavy user.
func TestNoLumpOfEarlierTraffic(t *testing.T) {
	h := newHarness(t)
	h.open(1, nodeA, 443, "203.0.113.9:1001", 900<<20)
	h.tick(0)
	h.send(1, 5<<20, 0)
	// The line, read from the log on start, was written an hour ago.
	h.line(h.now.Add(-time.Hour), 443, "203.0.113.9:1001", "ak-a", false)
	h.tick(2 * time.Second)
	// A connection whose line comes long after it opened counts from then.
	h.open(2, nodeA, 443, "203.0.113.9:1002", 0)
	for range 20 {
		h.send(2, 1<<20, 0)
		h.tick(2 * time.Second)
	}
	h.line(h.now, 443, "203.0.113.9:1002", "ak-b", false)
	h.send(2, 1<<20, 0)
	h.tick(2 * time.Second)
	h.expect(map[string][2]uint64{"ak-a": {5 << 20, 1}, "ak-b": {1 << 20, 1}})
	// A notice for a connection closed before the first dump is history too.
	h2 := newHarness(t)
	h2.l.close(sockdiag.Conn{Cookie: 9, Local: netip.AddrPortFrom(nodeA, 443), Remote: netip.MustParseAddrPort("203.0.113.9:1009"), BytesAcked: 50 << 20})
	h2.tick(0)
	h2.line(h2.now, 443, "203.0.113.9:1009", "ak-c", false)
	h2.expect(map[string][2]uint64{})
}

// Two open connections that a line cannot tell apart count for nobody, and
// neither is limited.
func TestAmbiguousConnectionsCountForNobody(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	h.open(1, nodeA, 443, "203.0.113.9:1001", 0)
	h.open(2, nodeB, 443, "203.0.113.9:1001", 0)
	h.open(3, nodeB, 443, "203.0.113.9:1002", 0)
	h.tick(2 * time.Second)
	h.line(h.now, 443, "203.0.113.9:1001", "ak-a", false)
	h.line(h.now, 443, "203.0.113.9:1002", "ak-b", false)
	h.send(1, 100, 0)
	h.send(2, 100, 0)
	h.send(3, 100, 0)
	h.tick(2 * time.Second)
	h.expect(map[string][2]uint64{"ak-b": {100, 1}})
	if len(h.flows("ak-a")) != 0 {
		t.Fatal("an ambiguous connection is limited")
	}
	// The same when the line comes first and both appear in one dump, in
	// either order.
	for _, order := range [][2]uint64{{4, 5}, {5, 4}} {
		h.line(h.now, 443, "203.0.113.9:1004", "ak-c", false)
		h.open(4, nodeA, 443, "203.0.113.9:1004", 0)
		h.open(5, nodeB, 443, "203.0.113.9:1004", 0)
		h.l.dump = func() ([]sockdiag.Conn, error) { return []sockdiag.Conn{h.kernel[order[0]], h.kernel[order[1]]}, nil }
		h.tick(time.Second)
		h.l.dump = h.dump
		if len(h.flows("ak-c")) != 0 {
			t.Fatalf("dump order %v bound an ambiguous connection", order)
		}
		h.shut(4, 0, true)
		h.shut(5, 0, true)
	}
}

// A bound connection keeps its user to the end, last bytes included, even
// when the log later names its address for someone else.
func TestBoundConnectionsKeepTheirUser(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	h.open(1, nodeA, 443, "203.0.113.9:1001", 0)
	h.tick(2 * time.Second)
	h.line(h.now, 443, "203.0.113.9:1001", "ak-a", false)
	h.line(h.now, 443, "203.0.113.9:1001", "ak-b", false)
	h.send(1, 700, 0)
	h.tick(2 * time.Second)
	h.shut(1, 300, true)
	h.expect(map[string][2]uint64{"ak-a": {1000, 0}})
}

// A client port the relay reuses for another user goes to the new user: a
// line never binds a connection that closed before it was written, or one
// that opened after it.
func TestReusedClientPortsFindTheirOwnConnection(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	h.open(1, nodeA, 443, "203.0.113.9:1001", 0)
	h.tick(2 * time.Second)
	h.line(h.now, 443, "203.0.113.9:1001", "ak-a", false)
	h.now = h.now.Add(time.Second)
	h.shut(1, 100, true)
	// The same address again, for another user, before the next dump.
	h.now = h.now.Add(500 * time.Millisecond)
	h.open(2, nodeA, 443, "203.0.113.9:1001", 0)
	h.line(h.now, 443, "203.0.113.9:1001", "ak-b", false)
	h.tick(time.Second)
	h.send(2, 200, 0)
	// An unbound connection that closed before its address's line was written.
	h.open(3, nodeA, 443, "203.0.113.9:1003", 0)
	h.tick(2 * time.Second)
	h.shut(3, 300, true)
	h.now = h.now.Add(time.Second)
	h.line(h.now, 443, "203.0.113.9:1003", "ak-c", false)
	// An old line of an address a new connection uses now.
	stale := h.now.Add(-time.Minute)
	h.open(4, nodeA, 443, "203.0.113.9:1004", 0)
	h.tick(2 * time.Second)
	h.line(stale, 443, "203.0.113.9:1004", "ak-d", false)
	h.send(4, 400, 0)
	h.tick(2 * time.Second)
	h.expect(map[string][2]uint64{"ak-a": {100, 0}, "ak-b": {200, 1}})
}

// A line waits a while for its TCP connection. On an inbound that also takes
// UDP, a line no TCP connection takes is a UDP flow for two hours.
func TestLinesWaitThenExpire(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	h.line(h.now, 443, "203.0.113.9:1001", "ak-a", false)
	h.line(h.now, 8443, "203.0.113.9:2001", "ak-u", true)
	h.tick(claimWait + time.Second)
	if len(h.l.claims) != 1 {
		t.Fatalf("claims %v", h.l.claims)
	}
	if got := h.flows("ak-u"); !reflect.DeepEqual(got, []string{"udp :8443 203.0.113.9:2001"}) {
		t.Fatalf("udp flows %v", got)
	}
	// A TCP connection on that inbound takes its line, and the user's further
	// lines for it are not UDP flows.
	h.open(5, nodeA, 8443, "203.0.113.9:2002", 0)
	h.tick(2 * time.Second)
	h.line(h.now, 8443, "203.0.113.9:2002", "ak-u", true)
	h.line(h.now, 8443, "203.0.113.9:2002", "ak-u", true)
	if got := h.flows("ak-u"); !reflect.DeepEqual(got, []string{"tcp 198.51.100.1:8443 203.0.113.9:2002", "udp :8443 203.0.113.9:2001"}) {
		t.Fatalf("flows %v", got)
	}
	h.tick(udpKeep)
	if got := h.flows("ak-u"); !reflect.DeepEqual(got, []string{"tcp 198.51.100.1:8443 203.0.113.9:2002"}) {
		t.Fatalf("flows after two hours %v", got)
	}
	// A line written while the connection was open but read after it closed
	// is still its own; one written after the close may be a UDP flow.
	written := h.now.Add(200 * time.Millisecond)
	h.now = h.now.Add(400 * time.Millisecond)
	h.shut(5, 1000, true)
	h.now = h.now.Add(600 * time.Millisecond)
	h.line(written, 8443, "203.0.113.9:2002", "ak-u", true)
	if got := h.flows("ak-u"); len(got) != 0 {
		t.Fatalf("a late line of a closed connection left %v", got)
	}
	h.line(h.now, 8443, "203.0.113.9:2002", "ak-u", true)
	if got := h.flows("ak-u"); !reflect.DeepEqual(got, []string{"udp :8443 203.0.113.9:2002"}) {
		t.Fatalf("a line after the close %v", got)
	}
}

// A dump taken before a close notice and read after it neither reopens the
// connection nor counts its bytes again; nor does a repeated notice.
func TestCloseNoticesCountTheLastBytesOnce(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	h.open(1, nodeA, 443, "203.0.113.9:1001", 1000)
	h.line(h.now, 443, "203.0.113.9:1001", "ak-a", false)
	h.tick(2 * time.Second)
	stale, _ := h.dump()
	h.send(1, 2000, 0)
	final := h.kernel[1]
	delete(h.kernel, 1)
	h.l.close(final)
	h.l.close(final)
	h.l.dump = func() ([]sockdiag.Conn, error) { return stale, nil }
	h.tick(2 * time.Second)
	h.l.dump = h.dump
	h.expect(map[string][2]uint64{"ak-a": {3000, 0}})
	if len(h.flows("ak-a")) != 0 {
		t.Fatal("a stale dump reopened a closed connection")
	}
}

// A client idle for a while leaves the report and comes back counting from
// zero, which Cloud reads as new traffic since the previous report.
func TestIdleClientsLeaveAndComeBackFromZero(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	h.open(1, nodeA, 443, "203.0.113.9:1001", 0)
	h.line(h.now, 443, "203.0.113.9:1001", "ak-a", false)
	h.tick(2 * time.Second)
	h.shut(1, 5000, true)
	h.tick(idleAfter - time.Second)
	h.expect(map[string][2]uint64{"ak-a": {5000, 0}})
	h.tick(2 * time.Second)
	h.expect(map[string][2]uint64{})
	h.open(2, nodeA, 443, "203.0.113.9:1002", 0)
	h.line(h.now, 443, "203.0.113.9:1002", "ak-a", false)
	h.send(2, 1000, 0)
	h.tick(2 * time.Second)
	h.expect(map[string][2]uint64{"ak-a": {1000, 1}})
	// Idle again, but back before the next report: Cloud never saw it leave,
	// so its count goes on rather than restarting.
	h.shut(2, 0, true)
	h.tick(idleAfter + time.Second)
	h.open(3, nodeA, 443, "203.0.113.9:1003", 0)
	h.line(h.now, 443, "203.0.113.9:1003", "ak-a", false)
	h.send(3, 500, 0)
	h.tick(2 * time.Second)
	h.expect(map[string][2]uint64{"ak-a": {1500, 1}})
}

// The usage says why the counts may be short, in the same epoch.
func TestUsageSaysWhenCountsMayBeShort(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	u := h.l.Usage()
	if u.Kernel != nil || u.Log != nil || len(u.Users) != 0 || u.Epoch == "" {
		t.Fatalf("healthy usage %+v", u)
	}
	h.logErr = errors.New("the Xray access log is off")
	if u := h.l.Usage(); u.Log == nil || u.Kernel != nil {
		t.Fatalf("log off: %+v", u)
	}
	h.dumpErr = errors.New("sock_diag: permission denied")
	h.tick(2 * time.Second)
	if u := h.l.Usage(); u.Kernel == nil || h.l.Err() == nil {
		t.Fatalf("kernel unavailable: %+v", u)
	}
	h.dumpErr, h.logErr = nil, nil
	h.tick(2 * time.Second)
	if again := h.l.Usage(); again.Kernel != nil || again.Log != nil || again.Epoch != u.Epoch || h.l.Err() != nil {
		t.Fatalf("after recovery %+v", again)
	}
}

// Counts past what JSON carries exactly start a new epoch.
func TestHugeCountsStartANewEpoch(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	epoch := h.l.Usage().Epoch
	h.open(1, nodeA, 443, "203.0.113.9:1001", 0)
	h.line(h.now, 443, "203.0.113.9:1001", "ak-a", false)
	h.send(1, maxSafeInt+1, 0)
	h.tick(2 * time.Second)
	u := h.l.Usage()
	if u.Epoch == epoch || len(u.Users) != 0 {
		t.Fatalf("usage %+v", u)
	}
	h.send(1, 10, 0)
	h.tick(2 * time.Second)
	h.expect(map[string][2]uint64{"ak-a": {10, 1}})
}

// A bound connection is told once it is gone for good: the line that bound
// it, when it closed, what it carried for its user, and the requests on it,
// including those logged before the kernel showed it. A connection nobody
// owns is not told.
func TestEndedConnectionsTellTheirLineAndRequests(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	// A multiplexed connection logs three requests before the next dump.
	first := lineAt(h.now, 443, "203.0.113.9:1001", "ak-a", false)
	first.Dest = accesslog.Dest{Network: "tcp", Host: "example.com", Port: 443}
	h.l.claim([]accesslog.Line{first, lineAt(h.now, 443, "203.0.113.9:1001", "ak-a", false),
		lineAt(h.now.Add(time.Millisecond), 443, "203.0.113.9:1001", "ak-a", false)})
	h.open(1, nodeA, 443, "203.0.113.9:1001", 300)
	h.open(2, nodeA, 443, "203.0.113.9:1002", 0)
	h.tick(2 * time.Second)
	h.send(1, 700, 40)
	h.line(h.now, 443, "203.0.113.9:1001", "ak-a", true)
	h.tick(2 * time.Second)
	h.shut(1, 1000, true)
	closed := h.now
	h.shut(2, 50, true)
	h.tick(keepClosed)
	if len(h.ended) != 0 {
		t.Fatalf("told before the connection was gone for good: %+v", h.ended)
	}
	h.tick(2 * time.Second)
	if len(h.ended) != 1 {
		t.Fatalf("ended %+v", h.ended)
	}
	e := h.ended[0]
	if e.Line.Email != "ak-a" || !e.At.Equal(closed) || e.Down != 2000 || e.Up != 40 || e.Requests != 4 {
		t.Fatalf("ended %+v", e)
	}
	// The newest of the waiting lines bound it, and it says where it went.
	if !e.Line.At.Equal(first.At.Add(time.Millisecond)) || e.Line.Dest != (accesslog.Dest{}) {
		t.Fatalf("bound by %+v", e.Line)
	}
	h.tick(time.Minute)
	if len(h.ended) != 1 {
		t.Fatalf("told twice: %+v", h.ended)
	}
}
