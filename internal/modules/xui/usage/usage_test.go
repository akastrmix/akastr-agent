package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/sockdiag"
)

type fakeOwners struct {
	err    error
	owners map[netip.AddrPort]string
}

func (f *fakeOwners) Err() error { return f.err }

// Owner knows connections to port 443 only, like a log that names one inbound.
func (f *fakeOwners) Owner(local, remote netip.AddrPort) (string, bool) {
	e, ok := f.owners[remote]
	return e, ok && local.Port() == 443
}

var (
	inbound  = netip.MustParseAddrPort("198.51.100.1:443")
	outbound = netip.MustParseAddrPort("198.51.100.1:50000")
	clientA  = netip.MustParseAddrPort("203.0.113.1:40001")
	clientB  = netip.MustParseAddrPort("203.0.113.2:40002")
	clientC  = netip.MustParseAddrPort("203.0.113.3:40003")
)

type fakeKernel struct {
	conns map[uint64]*sockdiag.Conn
	err   error
}

func (k *fakeKernel) dump() ([]sockdiag.Conn, error) {
	var out []sockdiag.Conn
	for _, c := range k.conns {
		out = append(out, *c)
	}
	return out, k.err
}

func (k *fakeKernel) open(cookie uint64, local, remote netip.AddrPort, acked, received uint64) {
	k.conns[cookie] = &sockdiag.Conn{Cookie: cookie, Local: local, Remote: remote, BytesAcked: acked, BytesReceived: received}
}

func noNotices(context.Context, func(sockdiag.Conn)) error { return nil }

func (k *fakeKernel) add(cookie uint64, acked, received uint64) {
	k.conns[cookie].BytesAcked += acked
	k.conns[cookie].BytesReceived += received
}

func lastReport(t *testing.T, m *Meter) report {
	t.Helper()
	var sent []protocol.ReportBody
	m.report(func(r protocol.ReportBody) error { sent = append(sent, r); return nil })
	if len(sent) != 1 || sent[0].Kind != "xui.usage" {
		t.Fatalf("sent %v", sent)
	}
	// The report goes out as JSON inside the envelope, not as encoded bytes.
	raw, ok := sent[0].Data.(json.RawMessage)
	if !ok {
		t.Fatalf("report data is %T", sent[0].Data)
	}
	r, err := protocol.DecodeStrict[report](raw, "xui.usage", "epoch", "error_code", "clients")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func totals(r report) map[string]total {
	out := map[string]total{}
	for _, c := range r.Clients {
		out[c.Email] = *c
	}
	return out
}

// History before the process started is never reported as new traffic, and
// bytes count only once the connection's user is known; both would otherwise
// arrive as one burst and look like a heavy user.
func TestCountsOnlyKnownTrafficSinceStart(t *testing.T) {
	k := &fakeKernel{conns: map[uint64]*sockdiag.Conn{}}
	owners := &fakeOwners{owners: map[netip.AddrPort]string{clientA: "ak-a"}}
	m := newMeter(owners, k.dump, noNotices)
	k.open(1, inbound, clientA, 900<<20, 10<<20) // long download before start
	m.poll()
	k.add(1, 5<<20, 1<<20)
	k.open(2, inbound, clientB, 2<<20, 0)  // user not known yet
	k.open(3, outbound, clientA, 7<<20, 0) // not an inbound connection
	k.open(4, inbound, clientC, 3<<20, 0)  // somebody else's client
	owners.owners[clientC] = "operator-own"
	m.poll()
	owners.owners[clientB] = "ak-b"
	k.add(2, 1<<20, 0)
	m.poll()
	got := totals(lastReport(t, m))
	want := map[string]total{
		"ak-a": {Email: "ak-a", Down: 5 << 20, Up: 1 << 20, Connections: 1},
		"ak-b": {Email: "ak-b", Down: 1 << 20, Connections: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("totals %v", got)
	}
	// A new connection after start counts from its first byte; a closed one
	// leaves its total and stops counting as a connection.
	k.open(5, inbound, netip.MustParseAddrPort("203.0.113.1:40005"), 4<<20, 0)
	owners.owners[netip.MustParseAddrPort("203.0.113.1:40005")] = "ak-a"
	delete(k.conns, 2)
	m.poll()
	got = totals(lastReport(t, m))
	if got["ak-a"].Down != 9<<20 || got["ak-a"].Connections != 2 || got["ak-b"].Down != 1<<20 || got["ak-b"].Connections != 0 {
		t.Fatalf("after new and closed connections %v", got)
	}
}

// What a connection carries after its last poll counts from the close notice,
// exactly once, even when a poll taken before the close is applied after it.
func TestCloseNoticesCountTheLastBytesOnce(t *testing.T) {
	k := &fakeKernel{conns: map[uint64]*sockdiag.Conn{}}
	owners := &fakeOwners{owners: map[netip.AddrPort]string{clientA: "ak-a", clientB: "ak-b"}}
	m := newMeter(owners, k.dump, noNotices)
	k.open(9, inbound, clientB, 50<<20, 0) // open before start
	gone := *k.conns[9]
	delete(k.conns, 9)
	gone.BytesAcked += 1 << 20
	m.close(gone) // closed before the first poll: its history is not new traffic
	m.poll()
	k.open(1, inbound, clientA, 1<<20, 0)
	m.poll()
	k.add(1, 2<<20, 0)
	stale, _ := k.dump()
	final := *k.conns[1]
	final.BytesAcked += 3 << 20
	delete(k.conns, 1)
	m.close(final)
	m.close(final) // a repeated notice adds nothing
	// A poll that listed the connection before it closed arrives late.
	m.dump = func() ([]sockdiag.Conn, error) { return stale, nil }
	m.poll()
	m.dump = k.dump
	m.poll()
	// A connection that opens and closes between two polls counts in full.
	m.close(sockdiag.Conn{Cookie: 2, Local: inbound, Remote: clientB, BytesAcked: 4 << 20, BytesReceived: 1 << 10})
	got := totals(lastReport(t, m))
	want := map[string]total{
		"ak-a": {Email: "ak-a", Down: 6 << 20},
		"ak-b": {Email: "ak-b", Down: 4 << 20, Up: 1 << 10},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("totals %v", got)
	}
}

// A client idle for a while leaves the report, and comes back counting from
// zero, which Cloud reads as new traffic since the previous report.
func TestIdleClientsLeaveAndComeBackFromZero(t *testing.T) {
	k := &fakeKernel{conns: map[uint64]*sockdiag.Conn{}}
	owners := &fakeOwners{owners: map[netip.AddrPort]string{clientA: "ak-a"}}
	m := newMeter(owners, k.dump, noNotices)
	clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return clock }
	m.poll()
	k.open(1, inbound, clientA, 5<<20, 0)
	m.poll()
	delete(k.conns, 1)
	clock = clock.Add(idleAfter - time.Second)
	m.poll()
	if got := totals(lastReport(t, m)); got["ak-a"].Down != 5<<20 {
		t.Fatalf("left too early %v", got)
	}
	clock = clock.Add(2 * time.Second)
	m.poll()
	if got := totals(lastReport(t, m)); len(got) != 0 {
		t.Fatalf("idle client kept %v", got)
	}
	k.open(2, inbound, clientA, 1<<20, 0)
	m.poll()
	if got := totals(lastReport(t, m)); got["ak-a"].Down != 1<<20 {
		t.Fatalf("returning client %v", got)
	}
}

// More active clients than one message holds make the report say so, rather
// than send a part or start over every poll and look healthy.
func TestTooManyActiveClientsReportUnavailable(t *testing.T) {
	k := &fakeKernel{conns: map[uint64]*sockdiag.Conn{}}
	owners := &fakeOwners{owners: map[netip.AddrPort]string{}}
	m := newMeter(owners, k.dump, noNotices)
	m.poll()
	epoch := lastReport(t, m).Epoch
	for i := 0; i <= maxClients; i++ {
		remote := netip.AddrPortFrom(netip.MustParseAddr("203.0.113.1"), uint16(1000+i))
		owners.owners[remote] = fmt.Sprintf("ak-c%d", i)
		k.open(uint64(100+i), inbound, remote, 1, 0)
	}
	m.poll()
	m.poll()
	r := lastReport(t, m)
	if r.ErrorCode != "xui_usage_unavailable" || len(r.Clients) != 0 || r.Epoch != epoch {
		t.Fatalf("report %s with %d clients in epoch %s", r.ErrorCode, len(r.Clients), r.Epoch)
	}
}

// A connection's last bytes belong to its own user even when, by the time the
// close notice arrives, a new connection from the same address has another.
func TestClosingBytesStayWithTheirUser(t *testing.T) {
	k := &fakeKernel{conns: map[uint64]*sockdiag.Conn{}}
	owners := &fakeOwners{owners: map[netip.AddrPort]string{clientA: "ak-a"}}
	m := newMeter(owners, k.dump, noNotices)
	m.poll()
	k.open(1, inbound, clientA, 1<<20, 0)
	m.poll()
	owners.owners[clientA] = "ak-b"
	m.close(sockdiag.Conn{Cookie: 1, Local: inbound, Remote: clientA, BytesAcked: 3 << 20})
	if got := totals(lastReport(t, m)); got["ak-a"].Down != 3<<20 || got["ak-b"].Down != 0 {
		t.Fatalf("totals %v", got)
	}
}

// Cloud must know when the counts may be short so it does not judge on them.
func TestReportsWhyCountsMayBeShort(t *testing.T) {
	k := &fakeKernel{conns: map[uint64]*sockdiag.Conn{}}
	owners := &fakeOwners{owners: map[netip.AddrPort]string{}}
	m := newMeter(owners, k.dump, noNotices)
	m.poll()
	r := lastReport(t, m)
	if r.ErrorCode != "" || len(r.Clients) != 0 || r.Epoch == "" {
		t.Fatalf("healthy report %+v", r)
	}
	owners.err = errors.New("the Xray access log is off")
	if r := lastReport(t, m); r.ErrorCode != "xui_log_unavailable" {
		t.Fatalf("log off reported %q", r.ErrorCode)
	}
	k.err = errors.New("sock_diag: permission denied")
	m.poll()
	if r := lastReport(t, m); r.ErrorCode != "xui_usage_unavailable" {
		t.Fatalf("kernel unavailable reported %q", r.ErrorCode)
	}
	k.err, owners.err = nil, nil
	m.poll()
	if again := lastReport(t, m); again.ErrorCode != "" || again.Epoch != r.Epoch {
		t.Fatalf("after recovery %+v", again)
	}
}

// Only the newest report waits for its acknowledgement.
func TestAcknowledgeSettlesTheNewestReport(t *testing.T) {
	m := newMeter(&fakeOwners{}, func() ([]sockdiag.Conn, error) { return nil, nil }, noNotices)
	var sent []protocol.ReportBody
	publish := func(r protocol.ReportBody) error { sent = append(sent, r); return nil }
	m.report(publish)
	m.report(publish)
	if ok, _ := m.Acknowledge(sent[0].ReportID); ok {
		t.Fatal("a replaced report was settled")
	}
	if ok, _ := m.Acknowledge(sent[1].ReportID); !ok {
		t.Fatal("the newest report was not settled")
	}
}
