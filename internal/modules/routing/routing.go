// Package routing keeps the routing rules and outbounds of the Xray that 3x-ui
// runs as Cloud computes them for this node. Cloud sends one complete target;
// the module writes it into the panel's Xray template, from which 3x-ui builds
// Xray each time it starts it, and then into the running Xray through its own
// api command, so a change takes effect without a restart. Everything else in
// the template stays as the operator set it. Turning the module off leaves
// the last template and running configuration to the panel.
package routing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/modules/xui"
	"github.com/akastrmix/akastr-agent/internal/panel"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const (
	Name = "routing"
	// Key is the one key Cloud names: the whole node.
	Key = "node"

	direct  = "direct"
	blocked = "blocked"
	// Outbounds Cloud owns carry this prefix; the module removes no other.
	ownedPrefix = "ak-o-"
	rulePrefix  = "ak-r-"
	// routingService lets Xray's api replace rules and outbounds.
	routingService = "RoutingService"
	// restartWait bounds how long a restarted Xray may take to answer.
	restartWait = 30 * time.Second

	codeInboundMissing  = "routing_inbound_missing"
	codeConfigInvalid   = "routing_config_invalid"
	codePanelFailed     = "routing_panel_failed"
	codeXrayUnavailable = "routing_xray_unavailable"
	codeApplyFailed     = "routing_apply_failed"
	codeRestartFailed   = "routing_restart_failed"
	codeTargetInvalid   = "target_invalid"
)

var ownedTag = regexp.MustCompile(`^ak-o-[a-z0-9-]{1,100}$`)

// Config has no fields: the module is on or off.
type Config struct{}

func ParseConfig(raw json.RawMessage) (Config, error) {
	return protocol.DecodeStrict[Config](raw, Name)
}

// Target is the node's whole routing: the default outbound, the outbounds
// Cloud owns and the rules in the order Xray tries them.
type Target struct {
	DefaultOutbound string     `json:"default_outbound"`
	Outbounds       []Outbound `json:"outbounds"`
	Rules           []Rule     `json:"rules"`
}

// Outbound is one Xray outbound object without its tag; a new tag stands for
// new contents.
type Outbound struct {
	Tag    string          `json:"tag"`
	Config json.RawMessage `json:"config"`
}

// Rule sends what matches every non-empty condition to Outbound. Inbounds are
// panel inbound IDs; Sources are where the client connects from, IPs where it
// connects to.
type Rule struct {
	Inbounds  []int    `json:"inbounds,omitempty"`
	Users     []string `json:"users,omitempty"`
	Sources   []string `json:"sources,omitempty"`
	Domains   []string `json:"domains,omitempty"`
	IPs       []string `json:"ips,omitempty"`
	Ports     string   `json:"ports,omitempty"`
	Networks  string   `json:"networks,omitempty"`
	Protocols []string `json:"protocols,omitempty"`
	Outbound  string   `json:"outbound"`
}

type targetDocument struct {
	DefaultOutbound string            `json:"default_outbound"`
	Outbounds       []json.RawMessage `json:"outbounds"`
	Rules           []json.RawMessage `json:"rules"`
}

var errTarget = errors.New("routing target is invalid")

func decodeTarget(raw json.RawMessage) (Target, error) {
	doc, err := protocol.DecodeStrict[targetDocument](raw, "routing target", "default_outbound", "outbounds", "rules")
	if err != nil {
		return Target{}, errTarget
	}
	t := Target{DefaultOutbound: doc.DefaultOutbound}
	tags := map[string]bool{direct: true, blocked: true}
	for _, item := range doc.Outbounds {
		o, err := protocol.DecodeStrict[Outbound](item, "routing outbound", "tag", "config")
		var config map[string]json.RawMessage
		if err != nil || !ownedTag.MatchString(o.Tag) || tags[o.Tag] || json.Unmarshal(o.Config, &config) != nil {
			return Target{}, errTarget
		}
		var proto string
		_, tagged := config["tag"]
		if tagged || json.Unmarshal(config["protocol"], &proto) != nil || proto == "" {
			return Target{}, errTarget
		}
		tags[o.Tag] = true
		t.Outbounds = append(t.Outbounds, o)
	}
	if t.DefaultOutbound == blocked || !tags[t.DefaultOutbound] {
		return Target{}, errTarget
	}
	for _, item := range doc.Rules {
		r, err := protocol.DecodeKnown[Rule](item, "routing rule")
		if err != nil || !tags[r.Outbound] || !validRule(r) {
			return Target{}, errTarget
		}
		t.Rules = append(t.Rules, r)
	}
	return t, nil
}

// validRule wants at least one condition and only owned users. What a
// condition says is Xray's to judge, before anything is written.
func validRule(r Rule) bool {
	if len(r.Inbounds)+len(r.Users)+len(r.Sources)+len(r.Domains)+len(r.IPs)+len(r.Protocols) == 0 && r.Ports == "" && r.Networks == "" {
		return false
	}
	for _, id := range r.Inbounds {
		if id < 1 {
			return false
		}
	}
	for _, user := range r.Users {
		if !xui.Owned(user) {
			return false
		}
	}
	for _, list := range [][]string{r.Sources, r.Domains, r.IPs, r.Protocols} {
		if slices.Contains(list, "") {
			return false
		}
	}
	return true
}

// Panel is what the module uses of the local 3x-ui panel.
type Panel interface {
	Inbounds(context.Context) ([]panel.Inbound, error)
	Template(context.Context) (panel.Template, error)
	SaveTemplate(context.Context, panel.Template) error
	Restart(context.Context) error
}

type Module struct {
	panel Panel
	xray  xray
	wait  time.Duration
	mu    sync.Mutex
}

var _ module.Desired = (*Module)(nil)

func New(p Panel) *Module {
	return &Module{panel: p, xray: cli{}, wait: restartWait}
}

func (m *Module) Validate(key string, raw json.RawMessage) error {
	if key != Key {
		return errors.New("routing target key must be node")
	}
	_, err := decodeTarget(raw)
	return err
}

// Apply converges the node to the target of its one key. Without a usable
// target the node keeps what it has.
func (m *Module) Apply(ctx context.Context, targets map[string]json.RawMessage) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw := targets[Key]
	if raw == nil {
		return map[string]string{}, nil
	}
	t, err := decodeTarget(raw)
	if err != nil {
		return map[string]string{Key: codeTargetInvalid}, nil
	}
	if code := m.apply(ctx, t); code != "" {
		return map[string]string{Key: code}, nil
	}
	return map[string]string{}, nil
}

// apply writes the template first, so whatever restarts Xray afterwards, the
// panel included, starts it with the target; then it brings the running Xray
// there. Every pass does all of it again: the rule table is always replaced.
func (m *Module) apply(ctx context.Context, t Target) string {
	running, err := m.xray.locate()
	if err != nil {
		return codeXrayUnavailable
	}
	inbounds, err := m.panel.Inbounds(ctx)
	if err != nil {
		return codePanelFailed
	}
	rules, err := xrayRules(t.Rules, inbounds)
	if err != nil {
		return codeInboundMissing
	}
	current, err := m.panel.Template(ctx)
	if err != nil {
		return codePanelFailed
	}
	next, err := managedTemplate(current.Config, t, rules)
	if err != nil {
		return codePanelFailed
	}
	if !sameManaged(current.Config, next) {
		// The same build Xray would run at its next start, checked before
		// anything is written.
		if err := m.xray.test(ctx, running, testConfig(running.config, next)); err != nil {
			if errors.Is(err, errInvalid) {
				return codeConfigInvalid
			}
			return codeXrayUnavailable
		}
		if err := m.panel.SaveTemplate(ctx, panel.Template{Config: next, OutboundTestURL: current.OutboundTestURL}); err != nil {
			return codePanelFailed
		}
		saved, err := m.panel.Template(ctx)
		if err != nil || !sameManaged(saved.Config, next) {
			return codePanelFailed
		}
	}
	// Only a restart adds the api service, Xray's default outbound is the
	// first one it started with, and the built-in outbounds change only with a
	// restart: someone may have edited them in the panel and restarted Xray.
	if !hasService(running.config, routingService) || firstOutbound(running.config) != t.DefaultOutbound ||
		!bytes.Equal(builtinOutbounds(running.config), builtinOutbounds(next)) {
		if running, err = m.restart(ctx, running); err != nil || firstOutbound(running.config) != t.DefaultOutbound {
			return codeRestartFailed
		}
	}
	return m.hotUpdate(ctx, running, t, apiRule(next), rules)
}

// hotUpdate adds the outbounds the rules need, replaces the whole rule table,
// then removes owned outbounds no rule uses any more. Open connections keep
// going.
func (m *Module) hotUpdate(ctx context.Context, running xrayRun, t Target, api json.RawMessage, rules []json.RawMessage) string {
	present, err := m.xray.outbounds(ctx, running)
	if err != nil {
		return codeXrayUnavailable
	}
	wanted := map[string]bool{}
	var add []json.RawMessage
	for _, o := range t.Outbounds {
		wanted[o.Tag] = true
		if !slices.Contains(present, o.Tag) {
			add = append(add, outboundJSON(o))
		}
	}
	if len(add) > 0 {
		if err := m.xray.addOutbounds(ctx, running, add); err != nil {
			return codeApplyFailed
		}
	}
	if err := m.xray.replaceRules(ctx, running, append([]json.RawMessage{api}, rules...)); err != nil {
		// Xray empties the table before it builds the new one, so a failure
		// part way leaves no rules, the api's own included. A restart from the
		// tested template recovers, unless another process already started
		// from it.
		if now, err := m.xray.locate(); err == nil && now.pid == running.pid && m.xray.rules(ctx, running) != nil {
			_, _ = m.restart(ctx, running)
		}
		return codeApplyFailed
	}
	var stale []string
	for _, tag := range present {
		if strings.HasPrefix(tag, ownedPrefix) && !wanted[tag] {
			stale = append(stale, tag)
		}
	}
	if len(stale) > 0 {
		if err := m.xray.removeOutbounds(ctx, running, stale); err != nil {
			return codeApplyFailed
		}
	}
	return ""
}

// restart has the panel start Xray again from the template and waits for the
// new process to answer on its api.
func (m *Module) restart(ctx context.Context, old xrayRun) (xrayRun, error) {
	if err := m.panel.Restart(ctx); err != nil {
		return xrayRun{}, err
	}
	deadline := time.Now().Add(m.wait)
	for {
		if running, err := m.xray.locate(); err == nil && running.pid != old.pid && m.xray.rules(ctx, running) == nil {
			if !hasService(running.config, routingService) {
				return xrayRun{}, errors.New("the restarted Xray has no routing service")
			}
			return running, nil
		}
		if time.Now().After(deadline) {
			return xrayRun{}, errors.New("the restarted Xray did not answer")
		}
		select {
		case <-ctx.Done():
			return xrayRun{}, ctx.Err()
		case <-time.After(m.wait / 60):
		}
	}
}

// xrayRules writes the rules as Xray takes them, inbounds by their current
// tag, each with a tag of its own.
func xrayRules(rules []Rule, inbounds []panel.Inbound) ([]json.RawMessage, error) {
	tags := map[int]string{}
	for _, ib := range inbounds {
		tags[ib.ID] = ib.Tag
	}
	out := make([]json.RawMessage, 0, len(rules))
	for i, r := range rules {
		var inboundTags []string
		for _, id := range r.Inbounds {
			tag, found := tags[id]
			if !found {
				return nil, errors.New("inbound missing")
			}
			inboundTags = append(inboundTags, tag)
		}
		// "type" is what 3x-ui writes on every rule; Xray accepts it as is.
		raw, _ := json.Marshal(struct {
			Type        string   `json:"type"`
			RuleTag     string   `json:"ruleTag"`
			InboundTag  []string `json:"inboundTag,omitempty"`
			User        []string `json:"user,omitempty"`
			SourceIP    []string `json:"sourceIP,omitempty"`
			Domain      []string `json:"domain,omitempty"`
			IP          []string `json:"ip,omitempty"`
			Port        string   `json:"port,omitempty"`
			Network     string   `json:"network,omitempty"`
			Protocol    []string `json:"protocol,omitempty"`
			OutboundTag string   `json:"outboundTag"`
		}{"field", rulePrefix + strconv.Itoa(i+1), inboundTags, r.Users, r.Sources, r.Domains, r.IPs, r.Ports, r.Networks, r.Protocols, r.Outbound})
		out = append(out, raw)
	}
	return out, nil
}
