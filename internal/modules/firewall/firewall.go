// Package firewall filters what reaches this node from outside and forwards
// ports to other nodes, as Cloud computes it (Cloud ADR 0109, 0110). The
// module owns one nftables table for IPv4 and IPv6; Cloud sends typed facts
// and the module renders the rules, whose skeleton and order are fixed here.
//
// Forwards are members, not rules: they follow every target first, whatever
// the rest of the target does, and IPv4 forwarding is turned on for them at
// run time and off again only when this Agent turned it on.
//
// Rules only change after a check that a new connection to Cloud still works
// before and after the change; a target that cuts the node off is undone and
// not installed again until Cloud changes it. The table outlives the process:
// stopping, updating or crashing leaves it, the next process finds it, and
// after a reboot the last confirmed target is installed from the state file
// before Cloud is reached. The table goes away only when Cloud says off, when
// the module is turned off (Leftovers) and on uninstall.
package firewall

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/state"
)

const (
	Name = "firewall"
	// Key is the one key Cloud names: the whole node.
	Key = "node"

	resolveInterval = 2 * time.Minute
	resolveTimeout  = 5 * time.Second
	// After installing, Cloud has this long to become reachable again.
	confirmWindow = 20 * time.Second
	confirmGap    = 4 * time.Second

	codeToolMissing      = "firewall_tool_missing"
	codeConntrackFailed  = "firewall_conntrack_failed"
	codeProbeUnavailable = "firewall_probe_unavailable"
	codeUnconfirmed      = "firewall_unconfirmed"
	codeApplyFailed      = "firewall_apply_failed"
	codeStateFailed      = "firewall_state_failed"
	codeForwardingFailed = "firewall_forwarding_failed"
)

// Config has no fields: the module is on or off.
type Config struct{}

func ParseConfig(raw json.RawMessage) (Config, error) {
	return protocol.DecodeStrict[Config](raw, Name)
}

// StateDirectory holds the module's state file; turning the module off
// removes it.
func StateDirectory(stateDir string) string { return filepath.Join(stateDir, Name) }

type Firewall struct {
	host    host
	file    *state.JSONFile
	missing func() bool
	resolve func(ctx context.Context, name string) ([]netip.Addr, error)
	now     func() time.Time
	// Timing is fixed in production; tests shorten it.
	confirmWindow, confirmGap time.Duration
	started                   chan struct{}

	mu    sync.Mutex
	state persisted
	// current is Cloud's newest target; its domains are the ones resolved.
	current *Target
}

var (
	_ module.Desired = (*Firewall)(nil)
	_ module.Worker  = (*Firewall)(nil)
)

func New(stateDir, controlEndpoint string) *Firewall {
	return newFirewall(newSystem(controlEndpoint), stateDir)
}

func newFirewall(h host, stateDir string) *Firewall {
	return &Firewall{
		host: h, file: state.NewJSONFile(filepath.Join(StateDirectory(stateDir), "state.json")),
		missing: toolMissing, resolve: lookup, now: time.Now,
		confirmWindow: confirmWindow, confirmGap: confirmGap,
		started: make(chan struct{}), state: emptyState(),
	}
}

func (f *Firewall) Validate(key string, raw json.RawMessage) error {
	if key != Key {
		return errInvalid
	}
	_, err := decodeTarget(raw)
	return err
}

// Started closes once the start rules ran. The control connection waits for it:
// a connection opened while a restarted machine has no table yet would be
// untracked when the table comes back, and the replies to it dropped until the
// Agent next sends something.
func (f *Firewall) Started() <-chan struct{} { return f.started }

// Apply converges the node to Cloud's target. Without the key the module owns
// nothing; an unusable target leaves the node as it is.
func (f *Firewall) Apply(ctx context.Context, targets map[string]json.RawMessage) (map[string]string, error) {
	select {
	case <-f.started:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	raw, named := targets[Key]
	if named && raw == nil {
		return nil, nil
	}
	target := off()
	if named {
		decoded, err := decodeTarget(raw)
		if err != nil {
			return map[string]string{Key: "target_invalid"}, nil
		}
		target = decoded
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	code := f.apply(ctx, target)
	switch {
	case code == "":
		return nil, nil
	case named:
		return map[string]string{Key: code}, nil
	default:
		return nil, errors.New(code)
	}
}

// wantsTable: observe and enforce filter, and forwards work in any mode; off
// without forwards is no table at all.
func wantsTable(mode string, forwards []Forward) bool { return mode != modeOff || len(forwards) > 0 }

// apply runs the steps of ADR 0109 in order, with the forwarding of ADR 0110
// ahead of them; f.mu is held.
func (f *Firewall) apply(ctx context.Context, t Target) string {
	if f.missing() {
		return codeToolMissing
	}
	f.current = &t
	installed, err := f.host.installed(ctx)
	if err != nil {
		slog.Warn("firewall table unreadable", "code", codeApplyFailed, "error", err.Error())
		return codeApplyFailed
	}
	if !wantsTable(t.Mode, t.Forwards) {
		if installed != "" {
			if err := f.host.load(ctx, remove()); err != nil {
				slog.Warn("firewall table not removed", "code", codeApplyFailed, "error", err.Error())
				return codeApplyFailed
			}
		}
		code := f.placed(ctx, []Forward{})
		if f.state.Confirmed != nil || f.state.Candidate != nil {
			f.state.Confirmed, f.state.Candidate = nil, nil
			code = cmp.Or(code, f.save())
		}
		return code
	}
	f.resolveMissing(ctx, t)
	forwards, forwardCode := f.openForwarding(ctx, t.Forwards)
	r := render(t, f.state.resolved(), f.placement(forwards))
	if _, digest, marked := parseMarker(installed); marked {
		// The forwards follow every target before the rules are compared, so
		// a node whose rules keep failing still forwards to where the landing
		// is now.
		if err := f.host.load(ctx, r.syncForwarding()); err != nil {
			slog.Warn("forwards not updated", "code", codeApplyFailed, "error", err.Error())
			return codeApplyFailed
		}
		forwardCode = cmp.Or(f.placed(ctx, forwards), forwardCode)
		if digest == r.digest {
			// Same rules: only the members may differ, and they change in place.
			if err := f.host.load(ctx, r.syncMembers()); err != nil {
				slog.Warn("firewall members not updated", "code", codeApplyFailed, "error", err.Error())
				return codeApplyFailed
			}
			if !reflect.DeepEqual(f.state.Confirmed, &t) || f.state.Candidate != nil {
				f.state.Confirmed, f.state.Candidate = &t, nil
				return cmp.Or(f.save(), forwardCode)
			}
			return forwardCode
		}
	}
	key := r.failureKey(t.Retry)
	if t.Mode != modeOff && key == f.state.Failed {
		return codeUnconfirmed
	}
	if code := f.raiseConntrack(ctx); code != "" {
		return code
	}
	if t.Mode == modeOff {
		// A table without filtering cannot cut the node off: no probe and no
		// record of failure, like removing the table.
		if err := f.host.load(ctx, r.install()); err != nil {
			slog.Warn("firewall rules refused", "code", codeApplyFailed, "error", err.Error())
			return codeApplyFailed
		}
		f.state.Confirmed, f.state.Candidate = &t, nil
		return cmp.Or(f.placed(ctx, forwards), f.save(), forwardCode)
	}
	if err := f.host.probe(ctx); err != nil {
		slog.Warn("Cloud unreachable before the firewall change; nothing changed", "code", codeProbeUnavailable, "error", err.Error())
		return codeProbeUnavailable
	}
	f.state.Candidate = &t
	if code := f.save(); code != "" {
		f.state.Candidate = nil
		return code
	}
	if err := f.host.load(ctx, r.install()); err != nil {
		slog.Warn("firewall rules refused", "code", codeApplyFailed, "error", err.Error())
		f.state.Candidate = nil
		_ = f.save()
		return codeApplyFailed
	}
	placedCode := f.placed(ctx, forwards)
	if f.confirm(ctx) {
		f.state.Confirmed, f.state.Candidate = &t, nil
		return cmp.Or(f.save(), placedCode, forwardCode)
	}
	slog.Error("Cloud unreachable after the firewall change; the previous rules are back", "code", codeUnconfirmed)
	f.restore(ctx)
	f.state.Failed, f.state.Candidate = key, nil
	_ = f.save()
	return codeUnconfirmed
}

func (f *Firewall) placement(forwards []Forward) placement {
	v4, v6 := f.host.transit()
	return placement{forwards: forwards, transit4: v4, transit6: v6}
}

// openForwarding turns IPv4 forwarding on when the target forwards and it is
// off. When it cannot, the table keeps the forwards it already has.
func (f *Firewall) openForwarding(ctx context.Context, wanted []Forward) ([]Forward, string) {
	if len(wanted) == 0 {
		return []Forward{}, ""
	}
	if on, err := f.host.forwarding(); err == nil && on {
		return wanted, ""
	}
	err := f.host.setForwarding(ctx, true)
	if err == nil {
		if on, readErr := f.host.forwarding(); readErr != nil || !on {
			err = errors.New("still off")
		}
	}
	if err != nil {
		slog.Warn("IPv4 forwarding not turned on; the forwards stay as they were", "code", codeForwardingFailed, "error", err.Error())
		return f.state.Forwards, codeForwardingFailed
	}
	f.state.ForwardingOpened = f.host.boot()
	return wanted, f.save()
}

// closeForwarding turns IPv4 forwarding off again once no forward is left,
// only when this Agent turned it on in this run of the machine: a machine
// that forwarded before (Docker and the like) keeps forwarding.
func (f *Firewall) closeForwarding(ctx context.Context) string {
	if f.state.ForwardingOpened == "" {
		return ""
	}
	if f.state.ForwardingOpened == f.host.boot() {
		if err := f.host.setForwarding(ctx, false); err != nil {
			slog.Warn("IPv4 forwarding not turned back off; retrying with the next target", "error", err.Error())
			return ""
		}
	}
	f.state.ForwardingOpened = ""
	return f.save()
}

// placed records the forwards the table now carries. A removed forward, or
// one whose target changed, leaves its old target to clean: its tracked
// connections would keep going there. The list is saved before cleaning, so a
// restart still cleans. With no forward left, forwarding goes back off.
func (f *Firewall) placed(ctx context.Context, forwards []Forward) string {
	forwards = slices.Clone(forwards)
	slices.SortFunc(forwards, func(a, b Forward) int {
		return cmp.Or(cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(a.Port, b.Port))
	})
	now := map[[2]any]Forward{}
	for _, forward := range forwards {
		now[[2]any{forward.Protocol, forward.Port}] = forward
	}
	stale := slices.Clone(f.state.Stale)
	for _, old := range f.state.Forwards {
		if current, found := now[[2]any{old.Protocol, old.Port}]; (!found || current != old) && !slices.Contains(stale, old) {
			stale = append(stale, old)
		}
	}
	// A target in use again is live: cleaning it would cut what it carries now.
	stale = slices.DeleteFunc(stale, func(old Forward) bool { return slices.Contains(forwards, old) })
	if len(stale) > maxStale {
		slog.Warn("too many old forward targets to clean; the oldest are left to time out", "dropped", len(stale)-maxStale)
		stale = stale[len(stale)-maxStale:]
	}
	code := ""
	if !slices.Equal(stale, f.state.Stale) || !slices.Equal(forwards, f.state.Forwards) {
		f.state.Forwards, f.state.Stale = forwards, stale
		code = f.save()
	}
	code = cmp.Or(code, f.clean(ctx))
	if len(forwards) == 0 {
		code = cmp.Or(code, f.closeForwarding(ctx))
	}
	return code
}

// clean deletes the tracked connections still going to each old target. One
// that fails stays for the next round; it never decides whether a target
// applied.
func (f *Firewall) clean(ctx context.Context) string {
	if len(f.state.Stale) == 0 {
		return ""
	}
	left := []Forward{}
	for _, old := range f.state.Stale {
		to := netip.AddrPortFrom(netip.MustParseAddr(old.To), uint16(old.ToPort))
		if err := f.host.forget(ctx, old.Protocol, old.Port, to); err != nil {
			slog.Error("old connections of a forward not cleaned; retrying with the next target",
				"protocol", old.Protocol, "port", old.Port, "error", err.Error())
			left = append(left, old)
		}
	}
	if len(left) == len(f.state.Stale) {
		return ""
	}
	f.state.Stale = left
	return f.save()
}

// confirm gives Cloud several chances to answer a new connection.
func (f *Firewall) confirm(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, f.confirmWindow)
	defer cancel()
	for {
		if f.host.probe(ctx) == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(f.confirmGap):
		}
	}
}

// restore puts the last confirmed rules back with the forwards in place (or
// only the forwards, or no table); it never probes.
func (f *Firewall) restore(ctx context.Context) {
	previous := off()
	if f.state.Confirmed != nil {
		previous = *f.state.Confirmed
	}
	if wantsTable(previous.Mode, f.state.Forwards) {
		err := f.host.load(ctx, render(previous, f.state.resolved(), f.placement(f.state.Forwards)).install())
		if err == nil {
			return
		}
		slog.Error("previous firewall rules not restored; removing the table", "error", err.Error())
	}
	if err := f.host.load(ctx, remove()); err != nil {
		slog.Error("firewall table not removed", "error", err.Error())
	}
}

// raiseConntrack lifts the tracked-connection limit to what this machine's
// memory allows, never lowering it; the rules depend on tracking.
func (f *Firewall) raiseConntrack(ctx context.Context) string {
	memory, err := f.host.memory()
	if err != nil {
		return codeConntrackFailed
	}
	want := conntrackLimit(memory)
	if current, ok := f.host.conntrackMax(); ok && current >= want {
		return ""
	}
	if err := f.host.raiseConntrack(ctx, want); err != nil {
		slog.Warn("connection tracking limit not raised", "code", codeConntrackFailed, "error", err.Error())
		return codeConntrackFailed
	}
	if current, ok := f.host.conntrackMax(); !ok || current < want {
		return codeConntrackFailed
	}
	return ""
}

func (f *Firewall) save() string {
	f.state.prune(f.current, f.state.Confirmed, f.state.Candidate)
	if err := f.file.Save(f.state); err != nil {
		slog.Error("firewall state not saved", "code", codeStateFailed, "error", err.Error())
		return codeStateFailed
	}
	return ""
}

// Run restores the table before Cloud is reached, then keeps the domains
// resolved.
func (f *Firewall) Run(ctx context.Context) error {
	f.start(ctx)
	ticker := time.NewTicker(resolveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			f.refresh(ctx)
		}
	}
}

// start applies the process start rules of ADR 0109. A state file that
// cannot be read installs nothing; the next target from Cloud replaces it.
func (f *Firewall) start(ctx context.Context) {
	defer close(f.started)
	f.mu.Lock()
	defer f.mu.Unlock()
	loaded := emptyState()
	if _, err := f.file.Load(&loaded); err != nil || loaded.validate() != nil {
		slog.Error("firewall state file unreadable; nothing restored until Cloud sends the target", "code", codeStateFailed)
		return
	}
	loaded.normalize()
	f.state = loaded
	if f.missing() {
		return
	}
	if opened := f.state.ForwardingOpened; opened != "" && opened != f.host.boot() {
		// The machine restarted, which turned forwarding off: whoever has it
		// on now is not this Agent.
		f.state.ForwardingOpened = ""
		_ = f.save()
	}
	installed, err := f.host.installed(ctx)
	if err != nil {
		slog.Error("firewall table unreadable at start", "error", err.Error())
		return
	}
	if installed == "" {
		// The machine restarted: put the confirmed rules back without
		// probing, since the network may not be up yet.
		if f.state.Candidate != nil {
			f.state.Candidate = nil
			_ = f.save()
		}
		previous := off()
		if f.state.Confirmed != nil {
			previous = *f.state.Confirmed
		}
		if !wantsTable(previous.Mode, f.state.Forwards) {
			return
		}
		// The order of every install: the tracking limit, forwarding, the table.
		if f.raiseConntrack(ctx) != "" {
			slog.Error("firewall not restored: connection tracking limit not raised", "code", codeConntrackFailed)
			return
		}
		f.openForwarding(ctx, f.state.Forwards)
		if err := f.host.load(ctx, render(previous, f.state.resolved(), f.placement(f.state.Forwards)).install()); err != nil {
			slog.Error("firewall not restored", "code", codeApplyFailed, "error", err.Error())
		}
		return
	}
	if candidate := f.state.Candidate; candidate != nil {
		// The previous process stopped before it confirmed what it installed.
		if f.confirm(ctx) {
			f.state.Confirmed = candidate
		} else {
			slog.Error("Cloud unreachable with the unconfirmed firewall rules; the previous rules are back", "code", codeUnconfirmed)
			f.state.Failed = render(*candidate, f.state.resolved(), placement{}).failureKey(candidate.Retry)
			f.restore(ctx)
		}
		f.state.Candidate = nil
		_ = f.save()
	}
}

// refresh resolves every domain again and updates the members in place.
func (f *Firewall) refresh(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.current
	if t == nil {
		t = f.state.Confirmed
	}
	if t == nil || t.Mode == modeOff || len(t.domains()) == 0 {
		return
	}
	changed := false
	for _, domain := range t.domains() {
		changed = f.resolveOne(ctx, domain) || changed
	}
	if !changed {
		return
	}
	_ = f.save()
	r := render(*t, f.state.resolved(), f.placement(f.state.Forwards))
	installed, err := f.host.installed(ctx)
	if _, digest, ok := parseMarker(installed); err != nil || !ok || digest != r.digest {
		return
	}
	if err := f.host.load(ctx, r.syncMembers()); err != nil {
		slog.Warn("firewall members not updated", "error", err.Error())
	}
}

// resolveMissing looks up the domains a target names for the first time, so
// a new domain counts from its first install.
func (f *Firewall) resolveMissing(ctx context.Context, t Target) {
	for _, domain := range t.domains() {
		if _, found := f.state.Domains[domain]; !found {
			f.resolveOne(ctx, domain)
		}
	}
}

// resolveOne records a domain's addresses and reports whether what is stored
// changed; the time of a repeated answer alone is not worth a write.
func (f *Firewall) resolveOne(ctx context.Context, domain string) bool {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	result, known := f.state.Domains[domain]
	if result.Addresses == nil {
		result.Addresses = []string{}
	}
	addresses, err := f.resolve(ctx, domain)
	if err != nil || len(addresses) == 0 {
		// Keep the last addresses: a failed lookup never widens or empties.
		changed := !known || !result.Failed
		result.Failed = true
		f.state.Domains[domain] = result
		return changed
	}
	texts := make([]string, 0, len(addresses))
	for _, address := range addresses {
		texts = append(texts, address.Unmap().String())
	}
	slices.Sort(texts)
	texts = slices.Compact(texts)
	if len(texts) > maxDomainAddresses {
		texts = texts[:maxDomainAddresses]
	}
	at := f.now().UTC()
	f.state.Domains[domain] = domainResult{Addresses: texts, ResolvedAt: &at}
	return !known || result.Failed || !slices.Equal(result.Addresses, texts)
}

func lookup(ctx context.Context, name string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", name)
}

// domains reports the last result of each domain the current target names.
func (f *Firewall) domains() []domainStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.current
	if t == nil {
		t = f.state.Confirmed
	}
	result := []domainStatus{}
	if t == nil {
		return result
	}
	for _, name := range t.domains() {
		d := f.state.Domains[name]
		addresses := d.Addresses
		if addresses == nil {
			addresses = []string{}
		}
		result = append(result, domainStatus{Name: name, Addresses: addresses, ResolvedAt: d.ResolvedAt, Failed: d.Failed})
	}
	slices.SortFunc(result, func(a, b domainStatus) int { return strings.Compare(a.Name, b.Name) })
	return result
}
