package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTC records tc calls and keeps the filters they leave, so listings
// match; out overrides other listings, and a batch fails while failBatch is set.
type fakeTC struct {
	calls     []string
	out       map[string]string
	failBatch bool
	filters   map[string]bool // "dev pref handle"
}

func (f *fakeTC) run(_ context.Context, stdin string, args ...string) (string, error) {
	call := strings.Join(args, " ")
	if stdin != "" {
		call += "\n" + stdin
	}
	f.calls = append(f.calls, call)
	if f.filters == nil {
		f.filters = map[string]bool{}
	}
	if args[0] == "-batch" {
		if f.failBatch {
			return "", errors.New("batch failed")
		}
		for _, line := range strings.Split(strings.TrimSpace(stdin), "\n") {
			f.apply(strings.Fields(line))
		}
		return "", nil
	}
	f.apply(args)
	if out, found := f.out[strings.Join(args, " ")]; found {
		return out, nil
	}
	if len(args) == 5 && args[0] == "filter" && args[1] == "show" && args[4] == "egress" {
		var lines []string
		for key := range f.filters {
			if dev, rest, _ := strings.Cut(key, " "); dev == args[3] {
				pref, handle, _ := strings.Cut(rest, " ")
				lines = append(lines, "filter protocol ip pref "+pref+" flower chain 0 handle 0x"+handle)
			}
		}
		return strings.Join(lines, "\n"), nil
	}
	return "", nil
}

// apply follows the filter changes of one tc command.
func (f *fakeTC) apply(a []string) {
	switch {
	case len(a) > 10 && a[0] == "filter" && a[1] == "add":
		f.filters[a[3]+" "+a[8]+" "+a[10]] = true
	case len(a) > 10 && a[0] == "filter" && a[1] == "del":
		delete(f.filters, a[3]+" "+a[8]+" "+a[10])
	case len(a) == 7 && a[0] == "filter" && a[1] == "del":
		for key := range f.filters {
			if strings.HasPrefix(key, a[3]+" "+a[6]+" ") {
				delete(f.filters, key)
			}
		}
	}
}

func (f *fakeTC) take() string {
	all := strings.Join(f.calls, "\n")
	f.calls = nil
	return all
}

type fakeLog struct {
	err   error
	conns map[string][]netip.AddrPort
}

func (l *fakeLog) Err() error { return l.err }
func (l *fakeLog) Connections(emails map[string]bool) map[string][]netip.AddrPort {
	out := map[string][]netip.AddrPort{}
	for email, addrs := range l.conns {
		if emails[email] {
			out[email] = addrs
		}
	}
	return out
}

func newTestLimiter(log *fakeLog, tc *fakeTC) *Limiter {
	l := New(log)
	l.tc = tc
	l.links = func() ([]string, error) { return []string{"eth0"}, nil }
	l.routes = func() ([]string, []string, error) { return []string{"eth0"}, []string{"eth0"}, nil }
	return l
}

// targets builds one-client groups named after their client, from key/rate pairs.
func targets(pairs ...any) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for i := 0; i < len(pairs); i += 2 {
		key, mbps := pairs[i].(string), pairs[i+1].(int)
		emails := `["` + key + `"]`
		if mbps == 0 {
			emails = "[]"
		}
		out[key] = json.RawMessage(fmt.Sprintf(`{"down_mbps":%d,"emails":%s}`, mbps, emails))
	}
	return out
}

func TestValidateAcceptsOnlyOwnedUsersAndSaneRates(t *testing.T) {
	l := New(&fakeLog{})
	for _, raw := range []string{`{"down_mbps":30,"emails":["ak-a1","ak-a2"]}`, `{"down_mbps":0,"emails":[]}`} {
		if err := l.Validate("sub-1", json.RawMessage(raw)); err != nil {
			t.Fatal(raw, err)
		}
	}
	many := `"ak-a0"`
	for i := 1; i <= maxEmails; i++ {
		many += fmt.Sprintf(`,"ak-a%d"`, i)
	}
	for _, raw := range []string{
		`{"down_mbps":30,"emails":["old-user"]}`,
		`{"down_mbps":30,"emails":["ak-a1","ak-a1"]}`,
		`{"down_mbps":10001,"emails":["ak-a1"]}`,
		`{"down_mbps":-1,"emails":[]}`,
		`{"down_mbps":30}`,
		`{"down_mbps":30,"emails":null}`,
		`{"down_mbps":30,"emails":["ak-a1"],"up_mbps":1}`,
		`{"down_mbps":30,"emails":[` + many + `]}`,
	} {
		if l.Validate("sub-1", json.RawMessage(raw)) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

// The clients of one group share one police action, so a subscription is
// held to its limit whichever of its clients carry the traffic; a client that
// Cloud names twice follows the lower limit.
func TestGroupsShareOneLimit(t *testing.T) {
	a, b, c := netip.MustParseAddrPort("203.0.113.1:1000"), netip.MustParseAddrPort("203.0.113.2:2000"), netip.MustParseAddrPort("203.0.113.3:3000")
	log := &fakeLog{conns: map[string][]netip.AddrPort{"ak-a": {a}, "ak-b": {b}, "ak-c": {c}}}
	tc := &fakeTC{out: map[string]string{}}
	l := newTestLimiter(log, tc)
	if _, err := l.Apply(context.Background(), map[string]json.RawMessage{
		"sub-1": json.RawMessage(`{"down_mbps":30,"emails":["ak-a","ak-b"]}`),
		"sub-2": json.RawMessage(`{"down_mbps":10,"emails":["ak-b","ak-c"]}`),
	}); err != nil {
		t.Fatal(err)
	}
	got := tc.take()
	if strings.Count(got, "actions replace") != 2 ||
		!strings.Contains(got, "dst_ip 203.0.113.1 dst_port 1000 action police index 2885681153") ||
		!strings.Contains(got, "dst_ip 203.0.113.2 dst_port 2000 action police index 2885681154") ||
		!strings.Contains(got, "dst_ip 203.0.113.3 dst_port 3000 action police index 2885681154") {
		t.Fatalf("groups not shared:\n%s", got)
	}
}

// The rules follow the limits and the users' connections with the smallest
// change, and a released police action is deleted on a later pass.
func TestLimiterConvergesRulesToLimitsAndConnections(t *testing.T) {
	a1, a2 := netip.MustParseAddrPort("203.0.113.1:1000"), netip.MustParseAddrPort("[2001:db8::1]:2000")
	log := &fakeLog{conns: map[string][]netip.AddrPort{"ak-a": {a1}}}
	tc := &fakeTC{out: map[string]string{}}
	l := newTestLimiter(log, tc)
	ctx := context.Background()

	codes, err := l.Apply(ctx, targets("ak-a", 30, "ak-b", 0))
	if err != nil || len(codes) != 0 {
		t.Fatalf("apply = %v %v", codes, err)
	}
	got := tc.take()
	for _, want := range []string{
		"qdisc add dev eth0 clsact",
		"actions replace action police rate 30mbit burst 375000 conform-exceed drop/continue index 2885681153",
		"filter add dev eth0 egress protocol ip pref 49001 handle 1 flower ip_proto tcp dst_ip 203.0.113.1 dst_port 1000 action police index 2885681153",
		"filter add dev eth0 egress protocol ip pref 49001 handle 2 flower ip_proto udp dst_ip 203.0.113.1 dst_port 1000 action police index 2885681153",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	// Nothing changed: no batch.
	if err := l.sync(ctx); err != nil || strings.Contains(tc.take(), "-batch") {
		t.Fatal("unchanged rules were rewritten")
	}
	// A new IPv6 connection adds only its filters.
	log.conns["ak-a"] = []netip.AddrPort{a1, a2}
	l.sync(ctx)
	got = tc.take()
	if !strings.Contains(got, "filter add dev eth0 egress protocol ipv6 pref 49002 handle 3 flower ip_proto tcp dst_ip 2001:db8::1 dst_port 2000") ||
		strings.Contains(got, "actions replace") || strings.Contains(got, "filter del") {
		t.Fatalf("new connection:\n%s", got)
	}
	// The same address now belongs to another limited user.
	log.conns = map[string][]netip.AddrPort{"ak-a": {a2}, "ak-b": {a1}}
	if _, err := l.Apply(ctx, targets("ak-a", 30, "ak-b", 10)); err != nil {
		t.Fatal(err)
	}
	got = tc.take()
	for _, want := range []string{
		"actions replace action police rate 10mbit burst 125000 conform-exceed drop/continue index 2885681154",
		"filter del dev eth0 egress protocol ip pref 49001 handle 1 flower",
		"dst_ip 203.0.113.1 dst_port 1000 action police index 2885681154",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	// Lifting a limit removes its filters, then its action.
	if _, err := l.Apply(ctx, targets("ak-a", 30)); err != nil {
		t.Fatal(err)
	}
	if got = tc.take(); strings.Count(got, "filter del") != 2 || strings.Contains(got, "filter add") {
		t.Fatalf("lift:\n%s", got)
	}
	l.sync(ctx)
	if got = tc.take(); !strings.Contains(got, "actions del action police index 2885681154") {
		t.Fatalf("released action not deleted:\n%s", got)
	}
}

// Rules removed by hand are rebuilt when Cloud's targets are applied again.
func TestApplyRebuildsRulesRemovedByHand(t *testing.T) {
	log := &fakeLog{conns: map[string][]netip.AddrPort{"ak-a": {netip.MustParseAddrPort("203.0.113.1:1000")}}}
	tc := &fakeTC{out: map[string]string{}}
	l := newTestLimiter(log, tc)
	if _, err := l.Apply(context.Background(), targets("ak-a", 30)); err != nil {
		t.Fatal(err)
	}
	tc.take()
	if _, err := l.Apply(context.Background(), targets("ak-a", 30)); err != nil || strings.Contains(tc.take(), "-batch") {
		t.Fatalf("intact rules were rebuilt: %v", err)
	}
	tc.filters = nil
	if _, err := l.Apply(context.Background(), targets("ak-a", 30)); err != nil {
		t.Fatal(err)
	}
	if got := tc.take(); !strings.Contains(got, "filter add dev eth0 egress protocol ip pref 49001 handle 1") {
		t.Fatalf("removed rules not rebuilt:\n%s", got)
	}
}

// Any failure removes every rule rather than leaving a half-applied set.
func TestLimiterRemovesEverythingOnFailure(t *testing.T) {
	log := &fakeLog{conns: map[string][]netip.AddrPort{"ak-a": {netip.MustParseAddrPort("203.0.113.1:1000")}}}
	tc := &fakeTC{out: map[string]string{"qdisc show dev eth0": "qdisc clsact ffff: parent ffff:fff1"}, failBatch: true}
	l := newTestLimiter(log, tc)
	codes, err := l.Apply(context.Background(), targets("ak-a", 30))
	if err != nil || codes["ak-a"] != codeApplyFailed {
		t.Fatalf("apply = %v %v", codes, err)
	}
	got := tc.take()
	if !strings.Contains(got, "filter del dev eth0 egress pref 49001") || !strings.Contains(got, "qdisc del dev eth0 clsact") {
		t.Fatalf("no cleanup after failure:\n%s", got)
	}
	// An unreadable log keeps known connections limited and reports itself.
	tc.failBatch, log.err = false, errors.New("access log is off")
	codes, err = l.Apply(context.Background(), targets("ak-a", 30))
	if err != nil || codes["ak-a"] != codeLogUnavailable || !strings.Contains(tc.take(), "filter add") {
		t.Fatalf("apply with log unavailable = %v %v", codes, err)
	}
}

func TestCleanupRemovesOnlyOwnRules(t *testing.T) {
	tc := &fakeTC{out: map[string]string{
		"qdisc show dev eth0":         "qdisc fq 0: root\nqdisc clsact ffff: parent ffff:fff1",
		"filter show dev eth0 egress": "filter protocol all pref 1 bpf chain 0",
		"actions ls action police":    "action order 0:  police 0xac000001 rate 30Mbit\naction order 1:  police 0x7 rate 1Mbit",
	}}
	if err := cleanup(context.Background(), tc, []string{"eth0", "lo"}); err != nil {
		t.Fatal(err)
	}
	got := tc.take()
	if strings.Contains(got, "qdisc del") || strings.Contains(got, "index 7") || !strings.Contains(got, "actions del action police index 2885681153") {
		t.Fatalf("cleanup touched others' rules or missed its own:\n%s", got)
	}
}

func TestDefaultRoutes(t *testing.T) {
	proc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proc, "net"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(proc, "net", "route"), []byte(
		"Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
			"eth0\t00000000\t01DEB52F\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"+
			"eth0\t00DEB52F\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"), 0o600)
	os.WriteFile(filepath.Join(proc, "net", "ipv6_route"), []byte(
		"00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003 ens4\n"+
			"00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200 lo\n"), 0o600)
	v4, v6, err := defaultRoutes(proc)
	if err != nil || strings.Join(v4, ",") != "eth0" || strings.Join(v6, ",") != "ens4" {
		t.Fatalf("defaultRoutes = %v %v %v", v4, v6, err)
	}
}
