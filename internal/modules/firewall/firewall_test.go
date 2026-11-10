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
	case strings.HasPrefix(script, remove()):
		h.table = script
	case h.table != "":
		// A member update: the rules stay, the members are the new ones.
		rules, _, _ := strings.Cut(h.table, "}\n}\n")
		h.table = rules + "}\n}\n" + strings.ReplaceAll(script, "flush set", "#flush set")
	}
	return nil
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
		"rules":[{"action":"allow","sources":"any","protocol":"tcp","ports":[[` + strconv.Itoa(port) + `,` + strconv.Itoa(port) + `]]}]}`)
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
	if err := h.load(context.Background(), render(candidate, nil).install()); err != nil {
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
		"rules":[{"action":"allow","sources":[],"protocol":"any","ports":[]}]}`
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
	rules := render(valid, nil).rules
	if !strings.Contains(rules, "ip saddr @m_x_4 accept") || regexp.MustCompile(`(?m)^\s+accept$`).MatchString(rules) {
		t.Fatalf("rule for an empty set:\n%s", rules)
	}
}
