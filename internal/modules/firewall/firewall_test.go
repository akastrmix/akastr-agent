package firewall

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost keeps the table as the last script installed into it.
type fakeHost struct {
	mu        sync.Mutex
	table     string
	loads     []string
	reachable func() bool
	probes    int
	conntrack int64
	// ipForward is net.ipv4.ip_forward; setFails makes turning it on fail.
	ipForward bool
	setFails  bool
	switches  []bool
	bootID    string
	// forgetFails makes cleaning fail; forgotten lists what was cleaned.
	forgetFails bool
	forgotten   []string
	// events orders what reached the machine.
	events []string
}

var markerText = regexp.MustCompile(`comment "(akastr:[^"]+)"`)

func (h *fakeHost) installed(context.Context) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.table == "" {
		return "", nil
	}
	return markerText.FindStringSubmatch(h.table)[1], nil
}

func (h *fakeHost) load(_ context.Context, script string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loads = append(h.loads, script)
	switch {
	case script == remove():
		h.table = ""
		h.events = append(h.events, "remove")
	case strings.HasPrefix(script, remove()):
		h.table = script
		h.events = append(h.events, "install")
	case h.table != "":
		// A member update: the rules stay, the members are the new ones.
		rules, _, _ := strings.Cut(h.table, "}\n}\n")
		h.table = rules + "}\n}\n" + strings.NewReplacer("flush set", "#flush set", "flush map", "#flush map").Replace(script)
	}
	return nil
}

func (h *fakeHost) forwarding() (bool, error) { return h.ipForward, nil }

func (h *fakeHost) setForwarding(_ context.Context, on bool) error {
	h.switches = append(h.switches, on)
	if on && h.setFails {
		return errors.New("systemd-run failed")
	}
	h.ipForward = on
	h.events = append(h.events, "forwarding "+strconv.FormatBool(on))
	return nil
}

func (h *fakeHost) boot() string {
	if h.bootID == "" {
		return "boot-1"
	}
	return h.bootID
}

func (h *fakeHost) transit() ([]string, []string) { return []string{"eth0"}, nil }

func (h *fakeHost) forget(_ context.Context, protocol string, port int, to netip.AddrPort) error {
	if h.forgetFails {
		return errors.New("netlink failed")
	}
	h.forgotten = append(h.forgotten, protocol+"/"+strconv.Itoa(port)+"->"+to.String())
	return nil
}

var forwardElements = regexp.MustCompile(`add element inet akastr_firewall forwards \{ ([^}]*) \}`)

// forwards reads the map entries the table carries now.
func (h *fakeHost) forwards() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.loads) - 1; i >= 0; i-- {
		script := h.loads[i]
		if script == remove() {
			return ""
		}
		if strings.HasPrefix(script, remove()) || strings.Contains(script, "flush map inet akastr_firewall forwards") {
			if m := forwardElements.FindStringSubmatch(script); m != nil {
				return m[1]
			}
			return ""
		}
	}
	return ""
}

func (h *fakeHost) conntrackMax() (int64, bool) { return h.conntrack, h.conntrack > 0 }

func (h *fakeHost) raiseConntrack(_ context.Context, limit int64) error {
	h.conntrack = limit
	return nil
}

func (h *fakeHost) probe(context.Context) error {
	h.mu.Lock()
	h.probes++
	h.mu.Unlock()
	if h.reachable == nil || h.reachable() {
		return nil
	}
	return errors.New("unreachable")
}

func (h *fakeHost) memory() (uint64, error) { return 1 << 30, nil }

func (h *fakeHost) installs() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, script := range h.loads {
		if strings.HasPrefix(script, remove()) && script != remove() {
			count++
		}
	}
	return count
}

func newTestFirewall(t *testing.T, h *fakeHost, dir string) *Firewall {
	t.Helper()
	f := newFirewall(h, dir)
	f.missing = func() bool { return false }
	f.confirmWindow, f.confirmGap = 50*time.Millisecond, 10*time.Millisecond
	f.resolve = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no resolver") }
	return f
}

func started(t *testing.T, f *Firewall) {
	t.Helper()
	f.start(context.Background())
}

func target(mode string, port int, extra ...string) json.RawMessage {
	sets := `{"home":{"addresses":["203.0.113.5"],"domains":[]}}`
	if len(extra) > 0 {
		sets = extra[0]
	}
	return json.RawMessage(`{"mode":"` + mode + `","retry":0,"sets":` + sets + `,"blocked":null,"exempt":null,"ceilings":[],
		"rules":[{"action":"allow","sources":"any","protocol":"tcp","ports":[[` + strconv.Itoa(port) + `,` + strconv.Itoa(port) + `]]}],"forwards":[]}`)
}

// forwarding gives a target forwards, as Cloud sends them.
func forwarding(raw json.RawMessage, forwards string) json.RawMessage {
	return json.RawMessage(strings.Replace(string(raw), `"forwards":[]`, `"forwards":[`+forwards+`]`, 1))
}

func forward(protocol string, port int, to string) string {
	return `{"protocol":"` + protocol + `","port":` + strconv.Itoa(port) + `,"to":"` + to + `","to_port":` + strconv.Itoa(port) + `}`
}

func apply(t *testing.T, f *Firewall, raw json.RawMessage) string {
	t.Helper()
	codes, err := f.Apply(context.Background(), map[string]json.RawMessage{Key: raw})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	return codes[Key]
}

func installedPort(h *fakeHost, port int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Contains(h.table, "tcp dport { "+strconv.Itoa(port)+" } accept")
}

// A target that cuts the node off is undone, is not installed again while it
// stays the same, and a changed target is.
func TestUnconfirmedTargetIsUndoneAndNotRepeated(t *testing.T) {
	h := &fakeHost{}
	f := newTestFirewall(t, h, t.TempDir())
	started(t, f)
	if code := apply(t, f, target("enforce", 22)); code != "" || !installedPort(h, 22) {
		t.Fatalf("first target: code %q, table %q", code, h.table)
	}
	// Reachable before the change, not after it.
	calls := 0
	h.reachable = func() bool { calls++; return calls == 1 }
	if code := apply(t, f, target("enforce", 23)); code != codeUnconfirmed {
		t.Fatalf("cut-off target code = %q", code)
	}
	if !installedPort(h, 22) || installedPort(h, 23) {
		t.Fatalf("previous rules not restored: %q", h.table)
	}
	before, probes := h.installs(), h.probes
	if code := apply(t, f, target("enforce", 23)); code != codeUnconfirmed || h.installs() != before || h.probes != probes {
		t.Fatalf("same failed target was tried again: code %q, installs %d → %d", code, before, h.installs())
	}
	h.reachable = nil
	if code := apply(t, f, target("enforce", 24)); code != "" || !installedPort(h, 24) {
		t.Fatalf("changed target: code %q, table %q", code, h.table)
	}
	// Cloud's retry counter makes the failed rules worth another try.
	retried := strings.Replace(string(target("enforce", 23)), `"retry":0`, `"retry":1`, 1)
	if code := apply(t, f, json.RawMessage(retried)); code != "" || !installedPort(h, 23) {
		t.Fatalf("retried target: code %q, table %q", code, h.table)
	}
}

func TestFirstTargetThatCutsTheNodeOffLeavesNoTable(t *testing.T) {
	h := &fakeHost{}
	f := newTestFirewall(t, h, t.TempDir())
	started(t, f)
	calls := 0
	h.reachable = func() bool { calls++; return calls == 1 }
	if code := apply(t, f, target("enforce", 22)); code != codeUnconfirmed || h.table != "" {
		t.Fatalf("code %q, table %q", code, h.table)
	}
}

func TestUnreachableCloudBeforeTheChangeChangesNothing(t *testing.T) {
	h := &fakeHost{reachable: func() bool { return false }}
	f := newTestFirewall(t, h, t.TempDir())
	started(t, f)
	if code := apply(t, f, target("enforce", 22)); code != codeProbeUnavailable || h.table != "" {
		t.Fatalf("code %q, table %q", code, h.table)
	}
}

// Off removes the table whatever Cloud's reachability.
func TestOffIgnoresTheProbe(t *testing.T) {
	h := &fakeHost{}
	f := newTestFirewall(t, h, t.TempDir())
	started(t, f)
	apply(t, f, target("enforce", 22))
	h.reachable = func() bool { return false }
	probes := h.probes
	if code := apply(t, f, target("off", 22)); code != "" || h.table != "" || h.probes != probes {
		t.Fatalf("code %q, table %q, probes %d → %d", code, h.table, probes, h.probes)
	}
}

func TestRestartWithoutTableRestoresConfirmedTargetWithoutProbing(t *testing.T) {
	dir := t.TempDir()
	h := &fakeHost{}
	f := newTestFirewall(t, h, dir)
	started(t, f)
	apply(t, f, target("enforce", 22))
	// The machine restarts: the table is gone, Cloud is not reachable yet.
	rebooted := &fakeHost{reachable: func() bool { return false }}
	next := newTestFirewall(t, rebooted, dir)
	started(t, next)
	if !installedPort(rebooted, 22) || rebooted.probes != 0 || rebooted.conntrack == 0 {
		t.Fatalf("table %q, probes %d, conntrack %d", rebooted.table, rebooted.probes, rebooted.conntrack)
	}
}

func TestRestartWithUnconfirmedRulesRestoresPreviousWhenCloudIsUnreachable(t *testing.T) {
	dir := t.TempDir()
	h := &fakeHost{}
	f := newTestFirewall(t, h, dir)
	started(t, f)
	apply(t, f, target("enforce", 22))
	// The process stops between installing a target and confirming it.
	candidate, _ := decodeTarget(target("enforce", 23))
	f.state.Candidate = &candidate
	if code := f.save(); code != "" {
		t.Fatal(code)
	}
	if err := h.load(context.Background(), render(candidate, nil, placement{}).install()); err != nil {
		t.Fatal(err)
	}
	h.reachable = func() bool { return false }
	next := newTestFirewall(t, h, dir)
	started(t, next)
	if !installedPort(h, 22) || installedPort(h, 23) || next.state.Candidate != nil || next.state.Failed == "" {
		t.Fatalf("table %q, state %+v", h.table, next.state)
	}
	// The same target is not installed again.
	h.reachable = nil
	if code := apply(t, next, target("enforce", 23)); code != codeUnconfirmed {
		t.Fatalf("code %q", code)
	}
}

func TestUnreadableStateInstallsNothingAndKeepsRunning(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(StateDirectory(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(StateDirectory(dir), "state.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &fakeHost{}
	f := newTestFirewall(t, h, dir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	<-f.started
	if len(h.loads) != 0 {
		t.Fatalf("installed from a broken state: %q", h.loads)
	}
	select {
	case err := <-done:
		t.Fatalf("Run stopped: %v", err)
	default:
	}
	// Cloud's target is applied and replaces the file.
	if code := apply(t, f, target("enforce", 22)); code != "" || !installedPort(h, 22) {
		t.Fatalf("code %q, table %q", code, h.table)
	}
	cancel()
	<-done
}

// A restarted process leaves the table until Cloud's complete target arrives.
func TestRestartKeepsTheTableUntilCloudSendsTheTarget(t *testing.T) {
	dir := t.TempDir()
	h := &fakeHost{}
	f := newTestFirewall(t, h, dir)
	started(t, f)
	apply(t, f, target("enforce", 22))
	loads := len(h.loads)
	next := newTestFirewall(t, h, dir)
	started(t, next)
	if len(h.loads) != loads || !installedPort(h, 22) {
		t.Fatalf("restart changed the table: %q", h.loads[loads:])
	}
}

func TestFailedLookupKeepsTheLastAddresses(t *testing.T) {
	h := &fakeHost{}
	f := newTestFirewall(t, h, t.TempDir())
	started(t, f)
	answer := []netip.Addr{netip.MustParseAddr("198.51.100.9")}
	var failing bool
	f.resolve = func(context.Context, string) ([]netip.Addr, error) {
		if failing {
			return nil, errors.New("lookup failed")
		}
		return answer, nil
	}
	sets := `{"home":{"addresses":[],"domains":["home.example.com"]}}`
	raw := json.RawMessage(strings.Replace(string(target("enforce", 22, sets)), `"sources":"any"`, `"sources":["home"]`, 1))
	if code := apply(t, f, raw); code != "" || !strings.Contains(h.table, "198.51.100.9") {
		t.Fatalf("code %q, table %q", code, h.table)
	}
	failing = true
	f.refresh(context.Background())
	answer = nil
	failing = false
	f.refresh(context.Background())
	if code := apply(t, f, raw); code != "" || !strings.Contains(h.table, "198.51.100.9") {
		t.Fatalf("failed or empty lookup dropped the address: %q", h.table)
	}
	status := f.domains()
	if len(status) != 1 || !status[0].Failed || status[0].Addresses[0] != "198.51.100.9" {
		t.Fatalf("domain status %+v", status)
	}
}

// Neither an empty set nor a missing source list can open the node to everyone.
func TestNoSourcesNeverMeansEveryone(t *testing.T) {
	empty := `{"mode":"enforce","retry":0,"sets":{"x":{"addresses":[],"domains":[]}},"blocked":null,"exempt":null,"ceilings":[],
		"rules":[{"action":"allow","sources":[],"protocol":"any","ports":[]}],"forwards":[]}`
	if _, err := decodeTarget([]byte(empty)); err == nil {
		t.Fatal("an empty source list was accepted")
	}
	for _, malformed := range []string{
		strings.Replace(empty, `"sources":[]`, `"sources":null`, 1),
		strings.Replace(empty, `"sources":[]`, `"sources":"all"`, 1),
		strings.Replace(empty, `"sources":[]`, `"sources":["missing"]`, 1),
		strings.Replace(empty, `,"blocked":null`, ``, 1),
	} {
		if _, err := decodeTarget([]byte(malformed)); err == nil {
			t.Fatalf("accepted %s", malformed)
		}
	}
	valid, err := decodeTarget([]byte(strings.Replace(empty, `"sources":[]`, `"sources":["x"]`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	rules := render(valid, nil, placement{}).rules
	if !strings.Contains(rules, "ip saddr @m_x_4 accept") || regexp.MustCompile(`(?m)^\s+accept$`).MatchString(rules) {
		t.Fatalf("rule for an empty set:\n%s", rules)
	}
}

// Forwarding goes back off only where this Agent turned it on; a machine that
// forwarded before (Docker) keeps forwarding.
func TestForwardingIsTurnedOffOnlyWhereTheAgentTurnedItOn(t *testing.T) {
	for _, before := range []bool{false, true} {
		h := &fakeHost{ipForward: before}
		f := newTestFirewall(t, h, t.TempDir())
		started(t, f)
		if code := apply(t, f, forwarding(target("observe", 22), forward("udp", 34820, "203.0.113.7"))); code != "" || !h.ipForward {
			t.Fatalf("before=%v: code %q, ip_forward %v", before, code, h.ipForward)
		}
		if opened := f.state.ForwardingOpened != ""; opened == before {
			t.Fatalf("before=%v: recorded as opened = %v", before, opened)
		}
		if code := apply(t, f, target("observe", 22)); code != "" || h.ipForward != before {
			t.Fatalf("before=%v: after the last forward went: code %q, ip_forward %v", before, code, h.ipForward)
		}
		if before && len(h.switches) != 0 {
			t.Fatalf("touched forwarding that was already on: %v", h.switches)
		}
	}
}

func TestForwardingThatCannotBeTurnedOnKeepsTheForwardsTheTableHas(t *testing.T) {
	h := &fakeHost{setFails: true}
	f := newTestFirewall(t, h, t.TempDir())
	started(t, f)
	a := forwarding(target("observe", 22), forward("tcp", 34820, "203.0.113.7"))
	if code := apply(t, f, a); code != codeForwardingFailed || h.table == "" || h.forwards() != "" {
		t.Fatalf("code %q, forwards %q, table installed %v", code, h.forwards(), h.table != "")
	}
	h.setFails = false
	if code := apply(t, f, a); code != "" || !strings.Contains(h.forwards(), "203.0.113.7") {
		t.Fatalf("code %q, forwards %q", code, h.forwards())
	}
	// Someone turns forwarding off and it cannot come back: the landing moves,
	// but the table keeps what it had rather than half of the change.
	h.ipForward, h.setFails = false, true
	b := forwarding(target("observe", 22), forward("tcp", 34820, "203.0.113.8"))
	if code := apply(t, f, b); code != codeForwardingFailed || !strings.Contains(h.forwards(), "203.0.113.7") {
		t.Fatalf("code %q, forwards %q", code, h.forwards())
	}
}

// A forward change only replaces members: no new table, no probe.
func TestForwardChangesFollowEvenARefusedTarget(t *testing.T) {
	h := &fakeHost{}
	f := newTestFirewall(t, h, t.TempDir())
	started(t, f)
	if code := apply(t, f, forwarding(target("enforce", 22), forward("tcp", 34820, "203.0.113.7"))); code != "" {
		t.Fatal(code)
	}
	installs, probes := h.installs(), h.probes
	if code := apply(t, f, forwarding(target("enforce", 22), forward("tcp", 34820, "203.0.113.8"))); code != "" ||
		h.installs() != installs || h.probes != probes || !strings.Contains(h.forwards(), "203.0.113.8") {
		t.Fatalf("code %q, installs %d → %d, probes %d → %d, forwards %q", code, installs, h.installs(), probes, h.probes, h.forwards())
	}
	// New rules that cut the node off are undone and refused from then on;
	// the landing's next address still reaches the table.
	calls := 0
	h.reachable = func() bool { calls++; return calls == 1 }
	if code := apply(t, f, forwarding(target("enforce", 23), forward("tcp", 34820, "203.0.113.9"))); code != codeUnconfirmed {
		t.Fatalf("cut-off target code = %q", code)
	}
	h.reachable = nil
	if code := apply(t, f, forwarding(target("enforce", 23), forward("tcp", 34820, "203.0.113.10"))); code != codeUnconfirmed ||
		!installedPort(h, 22) || !strings.Contains(h.forwards(), "203.0.113.10") {
		t.Fatalf("code %q, forwards %q, table %q", code, h.forwards(), h.table)
	}
}

// Mode off keeps the forwards working without filtering and, like removing
// the table, never waits on Cloud; without forwards it removes the table.
func TestModeOffKeepsForwardsWithoutProbing(t *testing.T) {
	h := &fakeHost{reachable: func() bool { return false }}
	f := newTestFirewall(t, h, t.TempDir())
	started(t, f)
	if code := apply(t, f, forwarding(target("off", 22), forward("udp", 34820, "203.0.113.7"))); code != "" ||
		h.probes != 0 || !strings.Contains(h.forwards(), "203.0.113.7") || strings.Contains(h.table, "chain input") {
		t.Fatalf("code %q, probes %d, forwards %q, table %q", code, h.probes, h.forwards(), h.table)
	}
	if code := apply(t, f, target("off", 22)); code != "" || h.table != "" || h.ipForward {
		t.Fatalf("code %q, table %q, ip_forward %v", code, h.table, h.ipForward)
	}
}

// Old connections go to the old target until cleaned; a failed cleaning is
// kept, across restarts, for the next round.
func TestCleaningOldConnectionsIsRetried(t *testing.T) {
	dir := t.TempDir()
	h := &fakeHost{}
	f := newTestFirewall(t, h, dir)
	started(t, f)
	apply(t, f, forwarding(target("observe", 22), forward("udp", 34820, "203.0.113.7")))
	h.forgetFails = true
	if code := apply(t, f, forwarding(target("observe", 22), forward("udp", 34820, "203.0.113.8"))); code != "" || len(f.state.Stale) != 1 {
		t.Fatalf("code %q, stale %+v", code, f.state.Stale)
	}
	h.forgetFails = false
	next := newTestFirewall(t, h, dir)
	started(t, next)
	if code := apply(t, next, forwarding(target("observe", 22), forward("udp", 34820, "203.0.113.8"))); code != "" ||
		len(next.state.Stale) != 0 || strings.Join(h.forgotten, ",") != "udp/34820->203.0.113.7:34820" {
		t.Fatalf("code %q, stale %+v, forgotten %v", code, next.state.Stale, h.forgotten)
	}
}

// After a reboot forwarding is off again and the Agent turns it on before
// the table comes back.
func TestRestartTurnsForwardingOnBeforeTheTable(t *testing.T) {
	dir := t.TempDir()
	h := &fakeHost{}
	f := newTestFirewall(t, h, dir)
	started(t, f)
	apply(t, f, forwarding(target("enforce", 22), forward("tcp", 34820, "203.0.113.7")))
	rebooted := &fakeHost{bootID: "boot-2", reachable: func() bool { return false }}
	next := newTestFirewall(t, rebooted, dir)
	started(t, next)
	if strings.Join(rebooted.events, ",") != "forwarding true,install" || !strings.Contains(rebooted.forwards(), "203.0.113.7") ||
		next.state.ForwardingOpened != "boot-2" {
		t.Fatalf("events %v, forwards %q, opened %q", rebooted.events, rebooted.forwards(), next.state.ForwardingOpened)
	}
}

// A forward the table cannot carry refuses the whole target.
func TestInvalidForwardsAreRefused(t *testing.T) {
	for _, bad := range []string{
		`null`,
		forward("tcp", 22, "203.0.113.7") + "," + forward("tcp", 22, "203.0.113.8"),
		forward("icmp", 22, "203.0.113.7"),
		forward("tcp", 0, "203.0.113.7"),
		forward("tcp", 22, "203.0.113.0/24"),
		forward("tcp", 22, "0.0.0.0"),
		forward("tcp", 22, "127.0.0.1"),
		forward("tcp", 22, "2001:db8::1"),
		`{"protocol":"tcp","port":22,"to":"203.0.113.7"}`,
	} {
		raw := forwarding(target("enforce", 22), bad)
		if bad == "null" {
			raw = json.RawMessage(strings.Replace(string(forwarding(target("enforce", 22), "")), `"forwards":[]`, `"forwards":null`, 1))
		}
		if _, err := decodeTarget(raw); err == nil {
			t.Fatalf("accepted forwards %s", bad)
		}
	}
}

// A bridge as a default-route interface is left out of the transit check:
// with br_netfilter, traffic between its ports enters and leaves through it.
func TestTransitLeavesBridgesOut(t *testing.T) {
	root := t.TempDir()
	proc, sys := filepath.Join(root, "proc"), filepath.Join(root, "sys")
	for _, dir := range []string{filepath.Join(proc, "net"), filepath.Join(sys, "class/net/eth0"), filepath.Join(sys, "class/net/vmbr0/bridge")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	route := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"vmbr0\t00000000\t01DEB52F\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth0\t00000000\t01DEB52F\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"
	ipv6 := "00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 00000400 00000001 00000000 00000001 wg0\n"
	if os.WriteFile(filepath.Join(proc, "net", "route"), []byte(route), 0o600) != nil ||
		os.WriteFile(filepath.Join(proc, "net", "ipv6_route"), []byte(ipv6), 0o600) != nil {
		t.Fatal("fixture")
	}
	v4, v6 := (&system{proc: proc, sys: sys}).transit()
	if strings.Join(v4, ",") != "eth0" || strings.Join(v6, ",") != "wg0" {
		t.Fatalf("transit %v %v", v4, v6)
	}
}
