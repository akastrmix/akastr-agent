package xui

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/akastrmix/akastr-agent/internal/module"
)

// Panel converges the owned clients of the local panel. Apply and snapshots
// share one lock, so reported traffic always matches the recorded resets.
type Panel struct {
	adapter *Adapter
	state   *localState
	mu      sync.Mutex
}

var _ module.Desired = (*Panel)(nil)

// NewPanel keeps the module's durable state in stateDir.
func NewPanel(cfg Config, stateDir string) (*Panel, error) {
	local, err := openState(stateDir)
	if err != nil {
		return nil, err
	}
	return &Panel{adapter: NewAdapter(cfg), state: local}, nil
}

func (p *Panel) Validate(key string, raw json.RawMessage) error {
	_, _, err := decodeTarget(key, raw)
	return err
}

// action is one panel write. identity addresses an existing client.
type action struct {
	kind     string // add, update or delete
	identity string
	client   map[string]any
}

func (p *Panel) Apply(ctx context.Context, raw map[string]json.RawMessage) (map[string]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	codes := map[string]string{}
	targets := map[int]Target{}
	keys := map[int]string{}
	// held inbounds are named without a usable target: left exactly as they are.
	held := map[int]bool{}
	for key, state := range raw {
		id, err := strconv.Atoi(key)
		if err != nil {
			codes[key] = "xui_target_invalid"
			continue
		}
		keys[id] = key
		if state == nil {
			held[id] = true
			continue
		}
		if _, target, err := decodeTarget(key, state); err != nil {
			codes[key] = "xui_target_invalid"
			held[id] = true
		} else {
			targets[id] = target
		}
	}
	fail := func(code string) (map[string]string, error) {
		for id := range targets {
			if codes[keys[id]] == "" {
				codes[keys[id]] = code
			}
		}
		return codes, errors.New(code)
	}
	inbounds, err := p.adapter.list(ctx)
	if err != nil {
		return fail(err.Error())
	}
	present := map[int]bool{}
	wrote := false
	var cleanup error
	for _, ib := range inbounds {
		present[ib.ID] = true
		if held[ib.ID] {
			continue
		}
		target, named := targets[ib.ID]
		changed, err := p.converge(ctx, ib, target, named)
		wrote = wrote || changed
		switch {
		case err != nil && named:
			codes[keys[ib.ID]] = err.Error()
		case err != nil && cleanup == nil:
			cleanup = err
		}
	}
	for id := range targets {
		if !present[id] {
			codes[keys[id]] = "xui_inbound_missing"
		}
	}
	// A Shadowsocks 2022 user added or changed through the panel can be
	// reported as written yet refuse connections until Xray reloads. The
	// pending mark survives a failed restart and a stop before it.
	if p.state.stored.RestartPending {
		if err := p.adapter.restart(ctx); err != nil {
			return fail("xui_restart_failed")
		}
		if err := p.state.setRestartPending(false); err != nil {
			return fail("xui_state_failed")
		}
	}
	if wrote {
		inbounds, err = p.adapter.list(ctx)
		if err != nil {
			return fail(err.Error())
		}
		for _, ib := range inbounds {
			target, named := targets[ib.ID]
			if !named || codes[keys[ib.ID]] != "" {
				continue
			}
			if actions, err := plan(ib, target); err != nil || len(actions) != 0 {
				codes[keys[ib.ID]] = "xui_unconfirmed"
			}
		}
	}
	// Reset marks of a held inbound's clients are unknown here; keep them all.
	if len(held) == 0 {
		emails := map[string]bool{}
		for _, target := range targets {
			for _, c := range target.Clients {
				emails[c.Email] = true
			}
		}
		if err := p.state.keepOnly(emails); err != nil {
			return fail("xui_state_failed")
		}
	}
	return codes, cleanup
}

// converge brings one inbound to its target; an inbound no key names keeps no
// owned client. It reports whether it wrote client settings. The target is
// checked against the inbound before any reset clears traffic.
func (p *Panel) converge(ctx context.Context, ib inbound, target Target, named bool) (bool, error) {
	if !named && !hasClients(ib) {
		// Protocols without clients hold nothing of ours.
		return false, nil
	}
	actions, err := plan(ib, target)
	if err != nil {
		return false, err
	}
	if named && p.resetsDue(ib, target) {
		if err := p.reset(ctx, &ib, target); err != nil {
			return false, err
		}
		if actions, err = plan(ib, target); err != nil {
			return false, err
		}
	}
	if len(actions) > 0 && ss2022(ib.Protocol, method(ib)) {
		if err := p.state.setRestartPending(true); err != nil {
			return false, errors.New("xui_state_failed")
		}
	}
	for i, a := range actions {
		switch a.kind {
		case "add":
			err = p.adapter.addClient(ctx, ib.ID, a.client)
		case "update":
			err = p.adapter.updateClient(ctx, ib.ID, a.identity, a.client)
		case "delete":
			err = p.adapter.deleteClient(ctx, ib.ID, a.identity)
		}
		if err != nil {
			return i > 0, errors.New("xui_write_failed")
		}
	}
	return len(actions) > 0, nil
}

func hasClients(ib inbound) bool {
	var s settings
	return json.Unmarshal([]byte(ib.Settings), &s) == nil && len(s.Clients) > 0
}

func (p *Panel) resetsDue(ib inbound, target Target) bool {
	for _, c := range target.Clients {
		if c.ResetSeq > p.state.reset(c.Email) {
			return true
		}
	}
	return false
}

// reset clears the traffic of each client whose reset Cloud asked for and
// this node has not executed yet. The panel's reset re-enables only its
// traffic record, never the client, so plan still decides enable afterwards.
func (p *Panel) reset(ctx context.Context, ib *inbound, target Target) error {
	existing := map[string]int{}
	for i, stat := range ib.ClientStats {
		existing[stat.Email] = i
	}
	for _, c := range target.Clients {
		if c.ResetSeq <= p.state.reset(c.Email) {
			continue
		}
		// A client created later starts empty, so only an existing one is reset.
		// Stopping between the reset and recording it repeats the reset once,
		// losing at most the traffic of that moment.
		if i, found := existing[c.Email]; found {
			if err := p.adapter.resetTraffic(ctx, ib.ID, c.Email); err != nil {
				return errors.New("xui_write_failed")
			}
			ib.ClientStats[i].Up, ib.ClientStats[i].Down = 0, 0
		}
		if err := p.state.setReset(c.Email, c.ResetSeq); err != nil {
			return errors.New("xui_state_failed")
		}
	}
	return nil
}

func method(ib inbound) string {
	var s settings
	_ = json.Unmarshal([]byte(ib.Settings), &s)
	return s.Method
}

// plan lists the writes that bring the owned clients of ib to target. A client
// whose traffic limit is used up stays disabled, as the panel left it: writing
// enable=true would only last until the panel disables it again. Raising the
// limit or resetting the traffic re-enables it.
func plan(ib inbound, target Target) ([]action, error) {
	var s settings
	if err := json.Unmarshal([]byte(ib.Settings), &s); err != nil || (s.Clients == nil && len(target.Clients) > 0) {
		return nil, errors.New("xui_inbound_invalid")
	}
	if err := compatible(ib.Protocol, s.Method, target); err != nil {
		return nil, err
	}
	field := credentialField(ib.Protocol)
	identity := func(c map[string]any) string {
		if ib.Protocol == "shadowsocks" {
			email, _ := c["email"].(string)
			return email
		}
		value, _ := c[field].(string)
		return value
	}
	used := map[string]int64{}
	for _, stat := range ib.ClientStats {
		used[stat.Email] = stat.Up + stat.Down
	}
	owned := map[string]map[string]any{}
	foreign := map[string]bool{}
	for _, c := range s.Clients {
		if email, _ := c["email"].(string); strings.HasPrefix(email, ownedPrefix) {
			owned[email] = c
		} else {
			foreign[identity(c)] = true
			if secret, _ := c[field].(string); secret != "" {
				foreign[secret] = true
			}
		}
	}
	// The panel addresses a client by its secret (Shadowsocks: email) and acts
	// on every client that matches, so a shared secret would reach a client
	// Cloud does not own.
	for _, c := range owned {
		if foreign[identity(c)] {
			return nil, errors.New("xui_client_conflict")
		}
	}
	for _, c := range target.Clients {
		if foreign[c.Credential] {
			return nil, errors.New("xui_client_conflict")
		}
	}
	var actions []action

	remaining := len(s.Clients)
	var unwanted []string
	for email := range owned {
		if !targetHas(target, email) {
			unwanted = append(unwanted, email)
		}
	}
	sort.Strings(unwanted)
	for _, email := range unwanted {
		c := owned[email]
		if remaining > 1 {
			actions = append(actions, action{kind: "delete", identity: identity(c)})
			remaining--
			continue
		}
		// The panel refuses to delete an inbound's last client; disabled, it serves nobody.
		if c["enable"] != false {
			updated := clone(c)
			updated["enable"] = false
			actions = append(actions, action{kind: "update", identity: identity(c), client: updated})
		}
	}

	for _, t := range target.Clients {
		enable := t.Enable && (t.TotalBytes == 0 || used[t.Email] < t.TotalBytes)
		fields := map[string]any{
			field: t.Credential, "enable": enable,
			"totalGB": float64(t.TotalBytes), "expiryTime": float64(0),
		}
		if ib.Protocol == "vless" {
			fields["flow"] = t.Flow
		}
		current, exists := owned[t.Email]
		if !exists {
			client := map[string]any{"email": t.Email, "subId": t.Email, "limitIp": float64(0), "tgId": float64(0), "reset": float64(0)}
			for key, value := range fields {
				client[key] = value
			}
			actions = append(actions, action{kind: "add", client: client})
			continue
		}
		updated := clone(current)
		changed := false
		for key, value := range fields {
			if !sameValue(current[key], value) {
				updated[key] = value
				changed = true
			}
		}
		if changed {
			actions = append(actions, action{kind: "update", identity: identity(current), client: updated})
		}
	}
	return actions, nil
}

func targetHas(target Target, email string) bool {
	for _, c := range target.Clients {
		if c.Email == email {
			return true
		}
	}
	return false
}

// sameValue compares a panel field with a target value; the panel may hold
// numbers as JSON numbers or strings.
func sameValue(current, want any) bool {
	if number, ok := want.(float64); ok {
		switch v := current.(type) {
		case float64:
			return v == number
		case string:
			parsed, err := strconv.ParseFloat(v, 64)
			return err == nil && parsed == number
		}
		return false
	}
	return reflect.DeepEqual(current, want)
}

// clone copies a client so fields the module does not manage are kept as they are.
func clone(c map[string]any) map[string]any {
	copied := make(map[string]any, len(c))
	for key, value := range c {
		copied[key] = value
	}
	return copied
}
