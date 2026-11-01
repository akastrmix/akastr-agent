package routing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/panel"
)

// fakeNode is a 3x-ui 2.9.4 panel with the Xray it runs, where the module
// depends on them: the template is read inside a JSON string and saved as a
// form, a missing outbound test URL is reset, a restart starts Xray from the
// template, and Xray's rule table is emptied before a failed rebuild.
type fakeNode struct {
	mu       sync.Mutex
	template string
	testURL  string
	inbounds []panel.Inbound
	events   []string
	// The running Xray.
	pid      int
	config   map[string]json.RawMessage
	live     []string // outbound tags
	table    []json.RawMessage
	apiUp    bool
	refuse   bool // xray -test fails
	breakAdd bool // the next rule table fails part way
}

func newFakeNode(t *testing.T, template string) (*fakeNode, *Module) {
	t.Helper()
	n := &fakeNode{template: template, testURL: "https://probe.test/204",
		inbounds: []panel.Inbound{{ID: 7, Tag: "inbound-7"}, {ID: 9, Tag: "inbound-9"}}}
	n.start()
	server := httptest.NewServer(n)
	t.Cleanup(server.Close)
	m := New(panel.New(server.URL+"/base/", "synthetic", "synthetic"))
	m.xray, m.wait = n, time.Second
	return n, m
}

func (n *fakeNode) event(name string) { n.events = append(n.events, name) }

// start runs Xray from the template, as the panel does.
func (n *fakeNode) start() {
	n.pid++
	var config map[string]json.RawMessage
	_ = json.Unmarshal([]byte(n.template), &config)
	config["inbounds"] = json.RawMessage(`[{"tag":"api","listen":"127.0.0.1","port":62789,"protocol":"tunnel"}]`)
	n.config = config
	var outbounds []struct {
		Tag string `json:"tag"`
	}
	_ = json.Unmarshal(config["outbounds"], &outbounds)
	n.live = nil
	for _, o := range outbounds {
		n.live = append(n.live, o.Tag)
	}
	var routing struct {
		Rules []json.RawMessage `json:"rules"`
	}
	_ = json.Unmarshal(config["routing"], &routing)
	n.table = routing.Rules
	n.apiUp = true
}

func (n *fakeNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	reply := func(obj any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": obj}) }
	if strings.HasSuffix(r.URL.Path, "/login") {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "synthetic", Path: "/"})
		reply(nil)
		return
	}
	if _, err := r.Cookie("session"); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch strings.TrimPrefix(r.URL.Path, "/base") {
	case "/panel/api/inbounds/list":
		reply(n.inbounds)
	case "/panel/xray/":
		text, _ := json.Marshal(map[string]any{"xraySetting": json.RawMessage(n.template),
			"inboundTags": []string{}, "outboundTestUrl": n.testURL})
		reply(string(text))
	case "/panel/xray/update":
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		n.template, n.testURL = r.PostForm.Get("xraySetting"), r.PostForm.Get("outboundTestUrl")
		if n.testURL == "" {
			n.testURL = "https://www.google.com/generate_204"
		}
		n.event("save")
		reply(nil)
	case "/panel/api/server/restartXrayService":
		n.event("restart")
		n.start()
		reply(nil)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (n *fakeNode) locate() (xrayRun, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return xrayRun{pid: strconv.Itoa(n.pid), api: "127.0.0.1:62789", config: n.config}, nil
}

func (n *fakeNode) test(_ context.Context, _ xrayRun, config []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.event("test")
	if n.refuse || !json.Valid(config) {
		return errInvalid
	}
	return nil
}

func (n *fakeNode) outbounds(context.Context, xrayRun) ([]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.live), nil
}

func (n *fakeNode) addOutbounds(_ context.Context, _ xrayRun, outbounds []json.RawMessage) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, raw := range outbounds {
		var o struct {
			Tag string `json:"tag"`
		}
		_ = json.Unmarshal(raw, &o)
		n.event("add " + o.Tag)
		n.live = append(n.live, o.Tag)
	}
	return nil
}

func (n *fakeNode) removeOutbounds(_ context.Context, _ xrayRun, tags []string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, tag := range tags {
		n.event("remove " + tag)
		n.live = slices.DeleteFunc(n.live, func(t string) bool { return t == tag })
	}
	return nil
}

func (n *fakeNode) replaceRules(_ context.Context, _ xrayRun, rules []json.RawMessage) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.event("rules")
	if n.breakAdd {
		n.breakAdd, n.table, n.apiUp = false, nil, false
		return errInvalid
	}
	n.table = rules
	return nil
}

func (n *fakeNode) rules(context.Context, xrayRun) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.apiUp {
		return errInvalid
	}
	return nil
}

func (n *fakeNode) savedTemplate(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	var config map[string]json.RawMessage
	if err := json.Unmarshal([]byte(n.template), &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func (n *fakeNode) takeEvents() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	events := n.events
	n.events = nil
	return events
}

// ruleTargets lists the running rules as outbound tags, the api's first.
func (n *fakeNode) ruleTargets() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, raw := range n.table {
		var r struct {
			OutboundTag string `json:"outboundTag"`
		}
		_ = json.Unmarshal(raw, &r)
		out = append(out, r.OutboundTag)
	}
	return out
}

// A template as the operator left it before the takeover.
const panelTemplate = `{
 "log": {"access": "./access.log", "loglevel": "warning"},
 "dns": {"servers": ["1.1.1.1"]},
 "api": {"tag": "api", "services": ["HandlerService", "LoggerService", "StatsService"]},
 "inbounds": [{"tag": "api", "listen": "127.0.0.1", "port": 62789, "protocol": "tunnel"}],
 "outbounds": [{"tag": "direct", "protocol": "freedom", "settings": {"domainStrategy": "UseIPv4"}},
  {"tag": "manual-us", "protocol": "shadowsocks"}, {"tag": "blocked", "protocol": "blackhole"}],
 "routing": {"domainStrategy": "IPIfNonMatch", "balancers": [{"tag": "b", "selector": ["manual"]}],
  "rules": [{"type": "field", "inboundTag": ["api"], "outboundTag": "api"},
   {"type": "field", "ip": ["geoip:private"], "outboundTag": "blocked"},
   {"type": "field", "domain": ["geosite:openai"], "outboundTag": "manual-us"}]}
}`

func target(t *testing.T, defaultOutbound string, outbounds []string, rules ...Rule) json.RawMessage {
	t.Helper()
	list := []Outbound{}
	for _, tag := range outbounds {
		list = append(list, Outbound{Tag: tag, Config: json.RawMessage(`{"protocol":"shadowsocks","settings":{"servers":[]}}`)})
	}
	if rules == nil {
		rules = []Rule{}
	}
	raw, err := json.Marshal(Target{DefaultOutbound: defaultOutbound, Outbounds: list, Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func apply(t *testing.T, m *Module, raw json.RawMessage) string {
	t.Helper()
	if err := m.Validate(Key, raw); err != nil {
		t.Fatalf("target rejected: %v", err)
	}
	codes, err := m.Apply(context.Background(), map[string]json.RawMessage{Key: raw})
	if err != nil {
		t.Fatal(err)
	}
	return codes[Key]
}

func TestTakeoverRewritesOnlyTheManagedParts(t *testing.T) {
	n, m := newFakeNode(t, panelTemplate)
	code := apply(t, m, target(t, "direct", []string{"ak-o-11111111-aaaaaaaa"},
		Rule{IPs: []string{"geoip:private"}, Outbound: "blocked"},
		Rule{Users: []string{"ak-zhang"}, Domains: []string{"geosite:openai"}, Outbound: "ak-o-11111111-aaaaaaaa"},
		Rule{Inbounds: []int{9}, Sources: []string{"geoip:cn"}, Networks: "tcp,udp", Outbound: "direct"}))
	if code != "" {
		t.Fatalf("takeover failed: %s", code)
	}
	// Tested before it is written; written before Xray restarts into it. The
	// first takeover restarts because only a restart adds the api service.
	if events := n.takeEvents(); !slices.Equal(events, []string{"test", "save", "restart", "rules"}) {
		t.Fatalf("takeover steps: %v", events)
	}
	saved := n.savedTemplate(t)
	before := map[string]json.RawMessage{}
	_ = json.Unmarshal([]byte(panelTemplate), &before)
	for _, key := range []string{"log", "dns", "inbounds"} {
		if string(canonical(saved[key])) != string(canonical(before[key])) {
			t.Errorf("template %s changed: %s", key, saved[key])
		}
	}
	if n.testURL != "https://probe.test/204" {
		t.Errorf("outbound test URL not kept: %s", n.testURL)
	}
	if got := firstOutbound(saved); got != "direct" {
		t.Errorf("default outbound %s", got)
	}
	var outbounds []map[string]any
	_ = json.Unmarshal(saved["outbounds"], &outbounds)
	var tags []string
	for _, o := range outbounds {
		tags = append(tags, o["tag"].(string))
	}
	if !slices.Equal(tags, []string{"direct", "blocked", "ak-o-11111111-aaaaaaaa"}) || outbounds[0]["settings"] != nil {
		t.Errorf("template outbounds: %s", saved["outbounds"])
	}
	var routing map[string]json.RawMessage
	_ = json.Unmarshal(saved["routing"], &routing)
	if string(routing["domainStrategy"]) != `"IPIfNonMatch"` || routing["balancers"] != nil {
		t.Errorf("routing section: %s", saved["routing"])
	}
	if !hasService(saved, routingService) || !hasService(saved, "StatsService") {
		t.Errorf("api section: %s", saved["api"])
	}
	// The panel's api rule stays first, in the template and in Xray.
	if got := n.ruleTargets(); !slices.Equal(got, []string{"api", "blocked", "ak-o-11111111-aaaaaaaa", "direct"}) {
		t.Fatalf("running rules: %v", got)
	}
	var rules []map[string]any
	_ = json.Unmarshal(routing["rules"], &rules)
	if rules[3]["inboundTag"].([]any)[0] != "inbound-9" || rules[3]["sourceIP"].([]any)[0] != "geoip:cn" ||
		rules[3]["network"] != "tcp,udp" || rules[2]["user"].([]any)[0] != "ak-zhang" ||
		rules[1]["ruleTag"] != "ak-r-1" {
		t.Errorf("template rules: %s", routing["rules"])
	}

	// The same target again changes nothing but replaces the table once more.
	if code := apply(t, m, target(t, "direct", []string{"ak-o-11111111-aaaaaaaa"},
		Rule{IPs: []string{"geoip:private"}, Outbound: "blocked"},
		Rule{Users: []string{"ak-zhang"}, Domains: []string{"geosite:openai"}, Outbound: "ak-o-11111111-aaaaaaaa"},
		Rule{Inbounds: []int{9}, Sources: []string{"geoip:cn"}, Networks: "tcp,udp", Outbound: "direct"})); code != "" {
		t.Fatal(code)
	}
	if events := n.takeEvents(); !slices.Equal(events, []string{"rules"}) {
		t.Fatalf("reapplying: %v", events)
	}
}

func TestChangesApplyLiveAndRemoveOnlyOwnedOutbounds(t *testing.T) {
	n, m := newFakeNode(t, panelTemplate)
	old, next := "ak-o-11111111-aaaaaaaa", "ak-o-11111111-bbbbbbbb"
	if code := apply(t, m, target(t, "direct", []string{old}, Rule{Users: []string{"ak-li"}, Outbound: old})); code != "" {
		t.Fatal(code)
	}
	n.takeEvents()
	// Outbounds someone added to the running Xray by hand are not Cloud's.
	n.mu.Lock()
	n.live = append(n.live, "operator-test")
	n.mu.Unlock()

	if code := apply(t, m, target(t, "direct", []string{next}, Rule{Users: []string{"ak-li"}, Outbound: next})); code != "" {
		t.Fatal(code)
	}
	// Added before the rules point at it, removed after they no longer do; no restart.
	if events := n.takeEvents(); !slices.Equal(events, []string{"test", "save", "add " + next, "rules", "remove " + old}) {
		t.Fatalf("live change: %v", events)
	}
	n.mu.Lock()
	live := slices.Clone(n.live)
	n.mu.Unlock()
	if !slices.Contains(live, "operator-test") || slices.Contains(live, old) || !slices.Contains(live, next) {
		t.Fatalf("running outbounds: %v", live)
	}
	if got := n.ruleTargets(); !slices.Equal(got, []string{"api", next}) {
		t.Fatalf("running rules: %v", got)
	}
}

func TestRefusedConfigurationWritesNothing(t *testing.T) {
	n, m := newFakeNode(t, panelTemplate)
	n.refuse = true
	if code := apply(t, m, target(t, "direct", nil, Rule{Domains: []string{"geosite:no-such"}, Outbound: "blocked"})); code != codeConfigInvalid {
		t.Fatalf("code %q", code)
	}
	if events := n.takeEvents(); !slices.Equal(events, []string{"test"}) {
		t.Fatalf("a refused configuration was written: %v", events)
	}
	if n.template != panelTemplate {
		t.Fatal("template changed")
	}
}

func TestDefaultOutboundChangeRestartsXray(t *testing.T) {
	n, m := newFakeNode(t, panelTemplate)
	exit := "ak-o-22222222-cccccccc"
	if code := apply(t, m, target(t, "direct", []string{exit})); code != "" {
		t.Fatal(code)
	}
	n.takeEvents()
	if code := apply(t, m, target(t, exit, []string{exit})); code != "" {
		t.Fatal(code)
	}
	if events := n.takeEvents(); !slices.Equal(events, []string{"test", "save", "restart", "rules"}) {
		t.Fatalf("default outbound change: %v", events)
	}
	n.mu.Lock()
	first := n.live[0]
	n.mu.Unlock()
	if first != exit {
		t.Fatalf("default outbound %s", first)
	}
}

func TestLostRuleTableIsRestoredByARestart(t *testing.T) {
	n, m := newFakeNode(t, panelTemplate)
	if code := apply(t, m, target(t, "direct", nil, Rule{IPs: []string{"geoip:private"}, Outbound: "blocked"})); code != "" {
		t.Fatal(code)
	}
	n.takeEvents()
	n.breakAdd = true
	if code := apply(t, m, target(t, "direct", nil, Rule{Protocols: []string{"bittorrent"}, Outbound: "blocked"})); code != codeApplyFailed {
		t.Fatalf("code %q", code)
	}
	// The emptied table took the api with it; Xray restarted from the template,
	// which already holds the new rules.
	if events := n.takeEvents(); !slices.Equal(events, []string{"test", "save", "rules", "restart"}) {
		t.Fatalf("recovery: %v", events)
	}
	if got := n.ruleTargets(); !slices.Equal(got, []string{"api", "blocked"}) || !n.apiUp {
		t.Fatalf("after recovery: %v api %v", got, n.apiUp)
	}
}

func TestMissingInboundWritesNothing(t *testing.T) {
	n, m := newFakeNode(t, panelTemplate)
	if code := apply(t, m, target(t, "direct", nil, Rule{Inbounds: []int{12}, Outbound: "blocked"})); code != codeInboundMissing {
		t.Fatalf("code %q", code)
	}
	if events := n.takeEvents(); len(events) != 0 {
		t.Fatalf("wrote for a missing inbound: %v", events)
	}
}

func TestInvalidTargetsAreRefused(t *testing.T) {
	m := New(nil)
	cases := map[string]string{
		"blocked default":      `{"default_outbound":"blocked","outbounds":[],"rules":[]}`,
		"unknown default":      `{"default_outbound":"ak-o-x","outbounds":[],"rules":[]}`,
		"rule to nowhere":      `{"default_outbound":"direct","outbounds":[],"rules":[{"users":["ak-a"],"outbound":"ak-o-x"}]}`,
		"rule without a match": `{"default_outbound":"direct","outbounds":[],"rules":[{"outbound":"direct"}]}`,
		"foreign user":         `{"default_outbound":"direct","outbounds":[],"rules":[{"users":["admin"],"outbound":"direct"}]}`,
		"empty source":         `{"default_outbound":"direct","outbounds":[],"rules":[{"sources":[""],"outbound":"blocked"}]}`,
		"tagged config":        `{"default_outbound":"direct","outbounds":[{"tag":"ak-o-x","config":{"tag":"y","protocol":"freedom"}}],"rules":[]}`,
		"no protocol":          `{"default_outbound":"direct","outbounds":[{"tag":"ak-o-x","config":{}}],"rules":[]}`,
		"foreign tag":          `{"default_outbound":"direct","outbounds":[{"tag":"us","config":{"protocol":"freedom"}}],"rules":[]}`,
		"unknown field":        `{"default_outbound":"direct","outbounds":[],"rules":[{"user":["ak-a"],"outbound":"direct"}]}`,
		"missing rules":        `{"default_outbound":"direct","outbounds":[]}`,
	}
	for name, raw := range cases {
		if m.Validate(Key, json.RawMessage(raw)) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if m.Validate("other", json.RawMessage(`{"default_outbound":"direct","outbounds":[],"rules":[]}`)) == nil {
		t.Error("a key other than node accepted")
	}
	if err := m.Validate(Key, json.RawMessage(`{"default_outbound":"direct","outbounds":[],"rules":[]}`)); err != nil {
		t.Errorf("the empty target refused: %v", err)
	}
}
