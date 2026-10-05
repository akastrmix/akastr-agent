package xui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/operation"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

func testPayload() Payload {
	return Payload{InboundID: 1, Protocol: "vless", Email: "synthetic-client", SubID: "synthetic-subscription", Credential: protocol.NewUUID(), Enable: true, TotalBytes: 100, TgID: 0}
}
func payloadJSON(p Payload) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"inbound_id": p.InboundID, "protocol": p.Protocol, "method": p.Method, "email": p.Email, "sub_id": p.SubID, "credential": p.Credential, "flow": p.Flow, "enable": p.Enable, "total_bytes": p.TotalBytes, "tg_id": p.TgID})
	return raw
}
func identityJSON(p Payload) json.RawMessage {
	var object map[string]any
	_ = json.Unmarshal(payloadJSON(p), &object)
	delete(object, "enable")
	delete(object, "total_bytes")
	delete(object, "tg_id")
	raw, _ := json.Marshal(object)
	return raw
}
func testAdapter(server *httptest.Server) *Adapter {
	return NewAdapter(Config{PanelURL: server.URL + "/panel-base/", Username: "synthetic", Password: "synthetic", SubscriptionURL: server.URL + "/sub/", SubscriptionHost: "node.example.net"})
}
func reply(w http.ResponseWriter, obj any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": obj})
}
func inboundJSON(c map[string]any) map[string]any {
	settings, _ := json.Marshal(map[string]any{"clients": []any{c}})
	return map[string]any{"id": 1, "protocol": "vless", "port": 443, "enable": true, "settings": string(settings), "streamSettings": `{"network":"tcp","security":"reality"}`}
}

func TestSessionExpiryHTMLAndBusinessFailure(t *testing.T) {
	for _, mode := range []string{"unauthorized", "not_found", "html", "business"} {
		t.Run(mode, func(t *testing.T) {
			logins, calls := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/login") {
					logins++
					http.SetCookie(w, &http.Cookie{Name: "session", Value: "synthetic", Path: "/"})
					reply(w, nil)
					return
				}
				calls++
				if mode == "business" {
					fmt.Fprint(w, `{"success":false,"msg":"credential must never escape"}`)
					return
				}
				if _, err := r.Cookie("session"); err != nil {
					if mode == "not_found" {
						w.WriteHeader(404)
					} else if mode == "unauthorized" {
						w.WriteHeader(401)
					} else {
						fmt.Fprint(w, "<html>login</html>")
					}
					return
				}
				reply(w, []any{})
			}))
			defer server.Close()
			_, err := testAdapter(server).list(context.Background())
			if mode == "business" {
				if err == nil || err.Error() != "xui_business_failed" || logins != 0 {
					t.Fatalf("business failure: %v, logins %d", err, logins)
				}
			} else if err != nil || logins != 1 || calls != 2 {
				t.Fatalf("session recovery: %v logins %d calls %d", err, logins, calls)
			}
		})
	}
}
func TestEnsurePreservesUnmanagedFieldsAndSkipsIdenticalUpdate(t *testing.T) {
	p := testPayload()
	c := map[string]any{"id": p.Credential, "email": p.Email, "subId": p.SubID, "enable": false, "totalGB": float64(5), "tgId": float64(0), "expiryTime": float64(0), "comment": "keep", "reverse": map[string]any{"tag": "existing"}, "unrecognized": []any{"keep"}}
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/get/1"):
			reply(w, inboundJSON(c))
		case strings.Contains(r.URL.Path, "/updateClient/"):
			writes++
			var body struct {
				Settings string `json:"settings"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var s settings
			_ = json.Unmarshal([]byte(body.Settings), &s)
			c = s.Clients[0]
			reply(w, nil)
		case strings.Contains(r.URL.Path, "/getClientTraffics/"):
			reply(w, map[string]any{"enable": true, "up": 1, "down": 2})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	h := New(testAdapter(server), "xui.client.ensure")
	offer := protocol.OperationOffer{Payload: payloadJSON(p)}
	if result := h.Run(context.Background(), offer); result.Outcome != "succeeded" {
		t.Fatal(result.Code)
	}
	if c["comment"] != "keep" || c["reverse"].(map[string]any)["tag"] != "existing" || c["unrecognized"].([]any)[0] != "keep" || c["id"] != p.Credential {
		t.Fatal("unmanaged fields or credentials changed")
	}
	if result := h.Run(context.Background(), offer); result.Outcome != "succeeded" || writes != 1 {
		t.Fatalf("identical target was rewritten: %s %d", result.Code, writes)
	}
	p.TotalBytes = 2
	p.TgID = 123456789
	offer.Payload = payloadJSON(p)
	if result := h.Run(context.Background(), offer); result.Outcome != "succeeded" || c["enable"] != false {
		t.Fatal("a metadata/limit update reopened an exhausted client")
	}
}
func TestResetUnknownIsNeverReexecuted(t *testing.T) {
	p := testPayload()
	c := map[string]any{"id": p.Credential, "email": p.Email, "subId": p.SubID, "enable": true}
	resets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/get/1") {
			reply(w, inboundJSON(c))
			return
		}
		if strings.Contains(r.URL.Path, "/resetClientTraffic/") {
			resets++
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	engine, err := operation.Open(operation.Options{StateFile: filepath.Join(t.TempDir(), "journal.json"), RecentLimit: 64})
	if err != nil {
		t.Fatal(err)
	}
	exec := operation.NewExecutor(engine)
	h := New(testAdapter(server), "xui.client.reset_traffic")
	offer := protocol.OperationOffer{CommandID: protocol.NewUUID(), CommandType: h.CommandType(), Payload: identityJSON(p), NotBefore: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}
	for range 2 {
		result, err := exec.Execute(context.Background(), offer, h.ExclusiveGroup(), h)
		if err != nil || result.Code != "xui_reset_unknown" {
			t.Fatalf("%s %v", result.Code, err)
		}
	}
	if resets != 1 {
		t.Fatalf("reset repeated %d", resets)
	}
	offer.CommandID = protocol.NewUUID()
	if _, err := engine.Begin(offer.CommandID, offer.CommandType, h.ExclusiveGroup()); err != nil {
		t.Fatal(err)
	}
	result, err := exec.Execute(context.Background(), offer, h.ExclusiveGroup(), h)
	if err != nil || result.Code != "xui_reset_unknown" || resets != 1 {
		t.Fatal("interrupted reset was reexecuted")
	}
}
