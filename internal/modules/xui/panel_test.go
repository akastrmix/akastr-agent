package xui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/state"
)

func jsonEncode(w io.Writer, value any) error { return json.NewEncoder(w).Encode(value) }

type fakeStat struct {
	up, down int64
	enable   bool
}

type fakeInbound struct {
	id       int
	protocol string
	settings map[string]any // without clients
	stream   map[string]any
	clients  []map[string]any
}

// fakePanel behaves like 3x-ui 2.9.4 where the module depends on it: the
// list carries traffic, the last client cannot be deleted, a reset clears the
// counters and re-enables only the traffic record.
type fakePanel struct {
	mu           sync.Mutex
	inbounds     []*fakeInbound
	stats        map[string]*fakeStat
	writes       []string
	restartFails bool
}

func newFakePanel(inbounds ...*fakeInbound) *fakePanel {
	p := &fakePanel{inbounds: inbounds, stats: map[string]*fakeStat{}}
	for _, ib := range inbounds {
		for _, c := range ib.clients {
			p.stats[c["email"].(string)] = &fakeStat{enable: c["enable"] != false}
		}
	}
	return p
}

func (p *fakePanel) inbound(id int) *fakeInbound {
	for _, ib := range p.inbounds {
		if ib.id == id {
			return ib
		}
	}
	return nil
}

func (p *fakePanel) client(id int, email string) map[string]any {
	for _, c := range p.inbound(id).clients {
		if c["email"] == email {
			return c
		}
	}
	return nil
}

func (p *fakePanel) identityIndex(ib *fakeInbound, identity string) int {
	for i, c := range ib.clients {
		key := credentialField(ib.protocol)
		if ib.protocol == "shadowsocks" {
			key = "email"
		}
		if c[key] == identity {
			return i
		}
	}
	return -1
}

func (p *fakePanel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/panel/")
	var body struct {
		ID       int    `json:"id"`
		Settings string `json:"settings"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	sent := func() map[string]any {
		var s settings
		_ = json.Unmarshal([]byte(body.Settings), &s)
		return s.Clients[0]
	}
	parts := strings.Split(path, "/")
	switch {
	case path == "login":
		reply(w, nil)
	case path == "panel/api/inbounds/list":
		list := []map[string]any{}
		for _, ib := range p.inbounds {
			s := map[string]any{"clients": ib.clients}
			for k, v := range ib.settings {
				s[k] = v
			}
			rawSettings, _ := json.Marshal(s)
			rawStream, _ := json.Marshal(ib.stream)
			stats := []map[string]any{}
			for _, c := range ib.clients {
				st := p.stats[c["email"].(string)]
				stats = append(stats, map[string]any{"email": c["email"], "up": st.up, "down": st.down, "enable": st.enable})
			}
			list = append(list, map[string]any{"id": ib.id, "remark": "r" + strconv.Itoa(ib.id), "protocol": ib.protocol,
				"listen": "", "port": 400 + ib.id, "enable": true, "settings": string(rawSettings),
				"streamSettings": string(rawStream), "clientStats": stats})
		}
		reply(w, list)
	case path == "panel/api/inbounds/addClient":
		c := sent()
		ib := p.inbound(body.ID)
		ib.clients = append(ib.clients, c)
		p.stats[c["email"].(string)] = &fakeStat{enable: c["enable"] == true}
		p.writes = append(p.writes, "add "+c["email"].(string))
		reply(w, nil)
	case strings.HasPrefix(path, "panel/api/inbounds/updateClient/"):
		c := sent()
		ib := p.inbound(body.ID)
		i := p.identityIndex(ib, parts[len(parts)-1])
		if i < 0 {
			w.Write([]byte(`{"success":false}`))
			return
		}
		ib.clients[i] = c
		p.stats[c["email"].(string)].enable = c["enable"] == true
		p.writes = append(p.writes, "update "+c["email"].(string))
		reply(w, nil)
	case len(parts) == 6 && parts[4] == "delClient":
		id, _ := strconv.Atoi(parts[3])
		ib := p.inbound(id)
		i := p.identityIndex(ib, parts[5])
		if i < 0 || len(ib.clients) == 1 {
			w.Write([]byte(`{"success":false,"msg":"no client remained in Inbound"}`))
			return
		}
		// 3x-ui removes every client the identity matches.
		kept := []map[string]any{}
		for _, c := range ib.clients {
			if p.identityIndex(&fakeInbound{protocol: ib.protocol, clients: []map[string]any{c}}, parts[5]) == 0 {
				p.writes = append(p.writes, "delete "+c["email"].(string))
				delete(p.stats, c["email"].(string))
			} else {
				kept = append(kept, c)
			}
		}
		ib.clients = kept
		reply(w, nil)
	case len(parts) == 6 && parts[4] == "resetClientTraffic":
		st := p.stats[parts[5]]
		st.up, st.down, st.enable = 0, 0, true
		p.writes = append(p.writes, "reset "+parts[5])
		reply(w, nil)
	case path == "panel/api/server/restartXrayService":
		if p.restartFails {
			w.Write([]byte(`{"success":false}`))
			return
		}
		p.writes = append(p.writes, "restart")
		reply(w, nil)
	default:
		w.WriteHeader(404)
	}
}

func (p *fakePanel) takeWrites() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	writes := p.writes
	p.writes = nil
	return writes
}

func startPanel(t *testing.T, fake *fakePanel) *Panel {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return openPanel(t, Config{PanelURL: server.URL + "/panel/", Username: "synthetic", Password: "synthetic"}, t.TempDir())
}

func openPanel(t *testing.T, cfg Config, stateDir string) *Panel {
	t.Helper()
	panel, err := NewPanel(cfg, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return panel
}

func vlessInbound(id int, clients ...map[string]any) *fakeInbound {
	return &fakeInbound{id: id, protocol: "vless", settings: map[string]any{"decryption": "none"},
		stream: map[string]any{"network": "tcp", "security": "reality"}, clients: clients}
}

func targets(t *testing.T, byKey map[string][]Client) map[string]json.RawMessage {
	t.Helper()
	raw := map[string]json.RawMessage{}
	for key, clients := range byKey {
		data, _ := json.Marshal(Target{Clients: clients})
		raw[key] = data
	}
	return raw
}

func owned(email string) Client {
	return Client{Email: email, Credential: protocol.NewUUID(), Flow: visionFlow, Enable: true}
}

func TestApplyConvergesOwnedClientsAndLeavesOthersAlone(t *testing.T) {
	foreign := map[string]any{"id": protocol.NewUUID(), "email": "admin-phone", "enable": true, "comment": "keep"}
	stale := map[string]any{"id": protocol.NewUUID(), "email": "ak-stale", "enable": true}
	fake := newFakePanel(vlessInbound(1, foreign, stale), vlessInbound(2, map[string]any{"id": protocol.NewUUID(), "email": "ak-orphan", "enable": true},
		map[string]any{"id": protocol.NewUUID(), "email": "other", "enable": true}))
	panel := startPanel(t, fake)
	a, b := owned("ak-a"), owned("ak-b")
	b.TotalBytes = 1000
	codes, _ := panel.Apply(context.Background(), targets(t, map[string][]Client{"1": {a, b}}))
	if len(codes) != 0 {
		t.Fatalf("codes %v", codes)
	}
	if fake.client(1, "ak-stale") != nil || fake.client(2, "ak-orphan") != nil {
		t.Fatal("owned clients without a target survived")
	}
	if c := fake.client(1, "admin-phone"); c == nil || c["comment"] != "keep" || fake.client(2, "other") == nil {
		t.Fatal("a client Cloud does not own was changed")
	}
	if c := fake.client(1, "ak-b"); c == nil || c["id"] != b.Credential || c["totalGB"] != float64(1000) || c["flow"] != visionFlow || c["enable"] != true {
		t.Fatalf("client not created as targeted: %v", c)
	}
	fake.takeWrites()
	if codes, _ := panel.Apply(context.Background(), targets(t, map[string][]Client{"1": {a, b}})); len(codes) != 0 || len(fake.takeWrites()) != 0 {
		t.Fatal("an unchanged target wrote to the panel")
	}
	a.Enable = false
	a.Credential = protocol.NewUUID()
	panel.Apply(context.Background(), targets(t, map[string][]Client{"1": {a, b}}))
	if c := fake.client(1, "ak-a"); c["enable"] != false || c["id"] != a.Credential {
		t.Fatalf("client not updated: %v", c)
	}
}

func TestExhaustedClientStaysDisabledUntilLimitRaisedOrReset(t *testing.T) {
	c := owned("ak-used")
	c.TotalBytes = 100
	fake := newFakePanel(vlessInbound(1, map[string]any{"id": "x", "email": "admin", "enable": true}))
	server := httptest.NewServer(fake)
	defer server.Close()
	cfg, stateDir := Config{PanelURL: server.URL + "/panel/", Username: "synthetic", Password: "synthetic"}, t.TempDir()
	panel := openPanel(t, cfg, stateDir)
	panel.Apply(context.Background(), targets(t, map[string][]Client{"1": {c}}))
	// The panel used up the limit and disabled the client, as 3x-ui does.
	fake.mu.Lock()
	fake.stats["ak-used"].up = 150
	fake.stats["ak-used"].enable = false
	fake.client(1, "ak-used")["enable"] = false
	fake.mu.Unlock()
	fake.takeWrites()
	if codes, _ := panel.Apply(context.Background(), targets(t, map[string][]Client{"1": {c}})); len(codes) != 0 || len(fake.takeWrites()) != 0 {
		t.Fatalf("exhausted client was reopened or reported: %v", codes)
	}
	c.ResetSeq = 1
	panel.Apply(context.Background(), targets(t, map[string][]Client{"1": {c}}))
	if writes := fake.takeWrites(); strings.Join(writes, ",") != "reset ak-used,update ak-used" || fake.client(1, "ak-used")["enable"] != true {
		t.Fatalf("reset did not clear and re-enable: %v", writes)
	}
	// The reset is recorded on disk, so a resent target never clears traffic again.
	openPanel(t, cfg, stateDir).Apply(context.Background(), targets(t, map[string][]Client{"1": {c}}))
	if writes := fake.takeWrites(); len(writes) != 0 {
		t.Fatalf("reset repeated: %v", writes)
	}
}

func TestLastClientIsDisabledInsteadOfDeleted(t *testing.T) {
	fake := newFakePanel(vlessInbound(1, map[string]any{"id": protocol.NewUUID(), "email": "ak-only", "enable": true}))
	panel := startPanel(t, fake)
	if codes, _ := panel.Apply(context.Background(), targets(t, map[string][]Client{})); len(codes) != 0 {
		t.Fatal(codes)
	}
	if c := fake.client(1, "ak-only"); c == nil || c["enable"] != false {
		t.Fatalf("last client: %v", c)
	}
}

func TestSS2022RestartsXrayOnceAfterWrites(t *testing.T) {
	ss := func(id int) *fakeInbound {
		return &fakeInbound{id: id, protocol: "shadowsocks", settings: map[string]any{"method": "2022-blake3-aes-128-gcm", "password": "c2VydmVyLWtleS0xNmJ5dA=="},
			stream: map[string]any{"network": "tcp", "security": "none"}, clients: []map[string]any{{"password": "AAAAAAAAAAAAAAAAAAAAAA==", "email": "admin", "enable": true}}}
	}
	fake := newFakePanel(ss(1), ss(2))
	panel := startPanel(t, fake)
	key := func(b byte) string { return "QUFBQUFBQUFBQUFBQUFB" + string(b) + "Q==" }
	desired := targets(t, map[string][]Client{
		"1": {{Email: "ak-s1", Credential: key('B'), Enable: true}},
		"2": {{Email: "ak-s2", Credential: key('C'), Enable: true}},
	})
	if codes, _ := panel.Apply(context.Background(), desired); len(codes) != 0 {
		t.Fatal(codes)
	}
	restarts := 0
	for _, w := range fake.takeWrites() {
		if w == "restart" {
			restarts++
		}
	}
	if restarts != 1 {
		t.Fatalf("restarts %d", restarts)
	}
	panel.Apply(context.Background(), desired)
	if writes := fake.takeWrites(); len(writes) != 0 {
		t.Fatalf("no-op restarted or wrote: %v", writes)
	}
}

func TestApplyReportsMissingAndChangedInbounds(t *testing.T) {
	fake := newFakePanel(vlessInbound(1, map[string]any{"id": "x", "email": "admin", "enable": true}))
	panel := startPanel(t, fake)
	wrongType := Client{Email: "ak-x", Credential: "not-a-uuid-but-long-enough", Enable: true}
	codes, _ := panel.Apply(context.Background(), targets(t, map[string][]Client{"1": {wrongType}, "9": {owned("ak-y")}}))
	if codes["1"] != "xui_inbound_changed" || codes["9"] != "xui_inbound_missing" || len(fake.takeWrites()) != 0 {
		t.Fatalf("codes %v", codes)
	}
}

func TestSnapshotKeepsSecretsOnTheNode(t *testing.T) {
	ib := vlessInbound(1, map[string]any{"id": "foreign-secret", "email": "admin", "enable": true})
	ib.settings["decryption"] = "mlkem768x25519plus.private"
	ib.stream["realitySettings"] = map[string]any{"privateKey": "reality-private", "mldsa65Seed": "seed", "settings": map[string]any{"publicKey": "pub"}}
	ib.stream["tlsSettings"] = map[string]any{"certificates": []any{map[string]any{"key": "inline-private"}}, "echServerKeys": "ech-private", "serverName": "a.example"}
	wg := &fakeInbound{id: 2, protocol: "wireguard", settings: map[string]any{"secretKey": "wg-private"}, stream: map[string]any{"network": "udp"}}
	fake := newFakePanel(ib, wg)
	panel := startPanel(t, fake)
	c := owned("ak-a")
	c.ResetSeq = 2
	panel.Apply(context.Background(), targets(t, map[string][]Client{"1": {c}}))
	data, err := NewSnapshots(panel).read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"foreign-secret", "private", "\"seed\"", "admin"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("snapshot contains %s: %s", secret, data)
		}
	}
	if !strings.Contains(string(data), `"publicKey":"pub"`) || !strings.Contains(string(data), `{"email":"ak-a","up":0,"down":0,"reset_seq":2}`) {
		t.Fatalf("snapshot lacks public fields or traffic: %s", data)
	}
}

func TestNamedInboundWithoutUsableTargetIsLeftAlone(t *testing.T) {
	fake := newFakePanel(vlessInbound(1, map[string]any{"id": protocol.NewUUID(), "email": "ak-kept", "enable": true},
		map[string]any{"id": protocol.NewUUID(), "email": "admin", "enable": true}))
	panel := startPanel(t, fake)
	if _, err := panel.Apply(context.Background(), map[string]json.RawMessage{"1": nil}); err != nil {
		t.Fatal(err)
	}
	if c := fake.client(1, "ak-kept"); c == nil || c["enable"] != true || len(fake.takeWrites()) != 0 {
		t.Fatal("an inbound still named by Cloud was cleaned")
	}
}

func TestFailedCleanupIsReportedForRetry(t *testing.T) {
	fake := newFakePanel(vlessInbound(1, map[string]any{"id": protocol.NewUUID(), "email": "ak-gone", "enable": true},
		map[string]any{"id": protocol.NewUUID(), "email": "admin", "enable": true}))
	panel := startPanel(t, fake)
	panel.adapter.cfg.PanelURL = "http://127.0.0.1:1/"
	if _, err := panel.Apply(context.Background(), map[string]json.RawMessage{}); err == nil {
		t.Fatal("an unreachable panel with nothing targeted reported success")
	}
}

func TestSharedSecretWithForeignClientBlocksWrites(t *testing.T) {
	shared := protocol.NewUUID()
	fake := newFakePanel(vlessInbound(1,
		map[string]any{"id": shared, "email": "ak-dup", "enable": true},
		map[string]any{"id": shared, "email": "admin-phone", "enable": true},
		map[string]any{"id": protocol.NewUUID(), "email": "admin-laptop", "enable": true}))
	panel := startPanel(t, fake)
	if _, err := panel.Apply(context.Background(), map[string]json.RawMessage{}); err == nil {
		t.Fatal("conflict not reported")
	}
	if fake.client(1, "admin-phone") == nil || len(fake.takeWrites()) != 0 {
		t.Fatal("a foreign client was deleted")
	}
	// A target reusing a foreign client's secret is refused before it exists.
	fake = newFakePanel(vlessInbound(1, map[string]any{"id": shared, "email": "admin-phone", "enable": true}))
	panel = startPanel(t, fake)
	clash := owned("ak-new")
	clash.Credential = shared
	codes, _ := panel.Apply(context.Background(), targets(t, map[string][]Client{"1": {clash}}))
	if codes["1"] != "xui_client_conflict" || len(fake.takeWrites()) != 0 {
		t.Fatalf("a client sharing a foreign secret was written: %v", codes)
	}
}

func TestFailedSS2022RestartIsRetriedWithoutNewWrites(t *testing.T) {
	fake := newFakePanel(&fakeInbound{id: 1, protocol: "shadowsocks", settings: map[string]any{"method": "2022-blake3-aes-128-gcm", "password": "c2VydmVyLWtleS0xNmJ5dA=="},
		stream: map[string]any{"network": "tcp", "security": "none"}, clients: []map[string]any{{"password": "AAAAAAAAAAAAAAAAAAAAAA==", "email": "admin", "enable": true}}})
	fake.restartFails = true
	server := httptest.NewServer(fake)
	defer server.Close()
	cfg, stateDir := Config{PanelURL: server.URL + "/panel/", Username: "synthetic", Password: "synthetic"}, t.TempDir()
	desired := targets(t, map[string][]Client{"1": {{Email: "ak-s1", Credential: "QUFBQUFBQUFBQUFBQUFBBQ==", Enable: true}}})
	if codes, _ := openPanel(t, cfg, stateDir).Apply(context.Background(), desired); codes["1"] != "xui_restart_failed" {
		t.Fatalf("codes %v", codes)
	}
	fake.restartFails = false
	fake.takeWrites()
	// A new process finds the restart still owed although the panel already matches.
	if codes, _ := openPanel(t, cfg, stateDir).Apply(context.Background(), desired); len(codes) != 0 {
		t.Fatal(codes)
	}
	if writes := fake.takeWrites(); len(writes) != 1 || writes[0] != "restart" {
		t.Fatalf("writes %v", writes)
	}
}

func TestIncompatibleTargetNeverResetsTraffic(t *testing.T) {
	fake := newFakePanel(vlessInbound(1, map[string]any{"id": protocol.NewUUID(), "email": "ak-x", "enable": true},
		map[string]any{"id": protocol.NewUUID(), "email": "admin", "enable": true}))
	panel := startPanel(t, fake)
	c := Client{Email: "ak-x", Credential: "not-a-uuid-but-long-enough", Enable: true, ResetSeq: 1}
	codes, _ := panel.Apply(context.Background(), targets(t, map[string][]Client{"1": {c}}))
	if codes["1"] != "xui_inbound_changed" || len(fake.takeWrites()) != 0 || panel.state.reset("ak-x") != 0 {
		t.Fatalf("traffic reset for a target the inbound cannot take: %v", codes)
	}
}

func TestResetRecordIsAdoptedOnlyOnceSaved(t *testing.T) {
	blocker := t.TempDir() + "/file"
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	local := &localState{file: state.NewJSONFile(blocker + "/state.json"), stored: stateFile{Schema: 1, Resets: map[string]int64{}}}
	if local.setReset("ak-x", 3) == nil || local.reset("ak-x") != 0 {
		t.Fatal("an unsaved reset was remembered, so it would be executed again after a restart")
	}
}

func TestTargetClientsNeedEveryField(t *testing.T) {
	for _, missing := range []string{"total_bytes", "enable", "reset_seq", "flow"} {
		client := map[string]any{"email": "ak-a", "credential": protocol.NewUUID(), "flow": "", "enable": true, "total_bytes": 100, "reset_seq": 0}
		delete(client, missing)
		raw, _ := json.Marshal(map[string]any{"clients": []any{client}})
		if new(Panel).Validate("1", raw) == nil {
			t.Errorf("target without %s accepted", missing)
		}
	}
}
