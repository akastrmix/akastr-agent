package xui

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type Handler struct {
	adapter *Adapter
	kind    string
}

func New(adapter *Adapter, kind string) *Handler { return &Handler{adapter: adapter, kind: kind} }
func (h *Handler) CommandType() string           { return h.kind }
func (h *Handler) ExclusiveGroup() string        { return "xui" }
func (h *Handler) Accepting() bool               { return true }
func (h *Handler) Validate(raw json.RawMessage) error {
	if h.kind == "xui.inbounds.list" {
		p, err := protocol.DecodeStrict[struct {
			AfterID int `json:"after_id"`
		}](raw, h.kind, "after_id")
		if err != nil || p.AfterID < 0 {
			return errors.New("xui list payload is invalid")
		}
		return nil
	}
	_, err := decodePayload(h.kind, raw)
	return err
}
func (h *Handler) Recover(offer protocol.OperationOffer) protocol.ExecutionResult {
	// Clearing traffic twice can erase new usage. Desired-state operations and reads
	// are repeatable; recovery rechecks identity before converging the same target.
	if h.kind == "xui.client.reset_traffic" {
		return failure("xui_reset_unknown")
	}
	return h.Run(context.Background(), offer)
}
func failure(code string) protocol.ExecutionResult {
	return protocol.ExecutionResult{Outcome: "failed", Code: code, Result: map[string]any{}}
}
func success(code string, result map[string]any) protocol.ExecutionResult {
	return protocol.ExecutionResult{Outcome: "succeeded", Code: code, Result: result}
}
func (h *Handler) Run(parent context.Context, offer protocol.OperationOffer) protocol.ExecutionResult {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	if h.Validate(offer.Payload) != nil {
		return failure("xui_payload_invalid")
	}
	var result protocol.ExecutionResult
	if h.kind == "xui.inbounds.list" {
		result = h.discover(ctx, offer.Payload)
	} else {
		p, _ := decodePayload(h.kind, offer.Payload)
		result = h.client(ctx, p)
	}
	raw, err := json.Marshal(result)
	if err != nil || len(raw) > 8192 {
		return failure("xui_result_too_large")
	}
	return result
}
func (h *Handler) discover(ctx context.Context, raw json.RawMessage) protocol.ExecutionResult {
	var p struct {
		AfterID int `json:"after_id"`
	}
	_ = json.Unmarshal(raw, &p)
	all, err := h.adapter.list(ctx)
	if err != nil {
		return failure(err.Error())
	}
	rows := []map[string]any{}
	var next any
	for _, ib := range all {
		if ib.ID <= p.AfterID {
			continue
		}
		var s settings
		var stream map[string]any
		if json.Unmarshal([]byte(ib.Settings), &s) != nil || json.Unmarshal([]byte(ib.StreamSettings), &stream) != nil {
			return failure("xui_inbound_invalid")
		}
		flow := ""
		if ib.Protocol == "vless" && stream["network"] == "tcp" && (stream["security"] == "reality" || stream["security"] == "tls") {
			flow = "xtls-rprx-vision"
		}
		row := map[string]any{"id": ib.ID, "remark": ib.Remark, "protocol": ib.Protocol, "port": ib.Port, "clients": len(s.Clients), "method": s.Method, "flow": flow}
		test, _ := json.Marshal(append(rows, row))
		if len(test) > 6000 || len(rows) == 20 {
			if len(rows) == 0 {
				return failure("xui_result_too_large")
			}
			next = rows[len(rows)-1]["id"]
			break
		}
		rows = append(rows, row)
	}
	return success("xui_inbounds", map[string]any{"inbounds": rows, "next_after_id": next})
}
func owned(s settings, p Payload) (map[string]any, error) {
	var found map[string]any
	for _, c := range s.Clients {
		if c["email"] != p.Email {
			if c["subId"] == p.SubID || c[credentialField(p.Protocol)] == p.Credential {
				return nil, errors.New("xui_client_conflict")
			}
			continue
		}
		if found != nil || c["subId"] != p.SubID || c[credentialField(p.Protocol)] != p.Credential {
			return nil, errors.New("xui_client_conflict")
		}
		found = c
	}
	return found, nil
}
func (h *Handler) client(ctx context.Context, p Payload) protocol.ExecutionResult {
	ib, s, err := h.adapter.get(ctx, p.InboundID)
	if err != nil {
		return failure(err.Error())
	}
	if ib.Protocol != p.Protocol || s.Method != p.Method {
		return failure("xui_inbound_changed")
	}
	c, err := owned(s, p)
	if err != nil {
		return failure(err.Error())
	}
	identity := p.Credential
	if p.Protocol == "shadowsocks" {
		identity = p.Email
	}
	updatePath := "updateClient/" + url.PathEscape(identity)
	body := func(c map[string]any) map[string]any {
		raw, _ := json.Marshal(map[string]any{"clients": []map[string]any{c}})
		return map[string]any{"id": p.InboundID, "settings": string(raw)}
	}
	retained := false
	effectiveEnable := p.Enable
	switch h.kind {
	case "xui.client.ensure":
		// Business eligibility does not replenish a spent quota. A tgId change,
		// for example, must not reopen an exhausted client even temporarily.
		if c != nil && p.Enable && p.TotalBytes > 0 {
			traffic, readErr := h.adapter.traffic(ctx, p.Email)
			if readErr != nil {
				return failure(readErr.Error())
			}
			if number(traffic["up"])+number(traffic["down"]) >= p.TotalBytes {
				effectiveEnable = false
			}
		}
		fresh := c == nil
		if fresh {
			c = map[string]any{"email": p.Email, "subId": p.SubID, credentialField(p.Protocol): p.Credential, "flow": p.Flow, "limitIp": 0, "reset": 0}
			if p.Protocol == "vmess" {
				c["security"] = "auto"
			}
		}
		fields := map[string]any{"enable": effectiveEnable, "totalGB": float64(p.TotalBytes), "tgId": float64(p.TgID), "expiryTime": float64(0)}
		changed := fresh
		for key, value := range fields {
			if !reflect.DeepEqual(c[key], value) {
				c[key] = value
				changed = true
			}
		}
		if changed {
			path := updatePath
			if fresh {
				path = "addClient"
			}
			if err = h.adapter.write(ctx, path, body(c)); err != nil {
				return failure(err.Error())
			}
		}
	case "xui.client.delete":
		if c != nil {
			if len(s.Clients) == 1 {
				c["enable"] = false
				retained = true
				err = h.adapter.write(ctx, updatePath, body(c))
			} else {
				err = h.adapter.write(ctx, strconv.Itoa(p.InboundID)+"/delClient/"+url.PathEscape(identity), map[string]any{})
			}
			if err != nil {
				return failure(err.Error())
			}
		}
	case "xui.client.reset_traffic":
		if c == nil {
			return failure("xui_client_missing")
		}
		if err = h.adapter.write(ctx, strconv.Itoa(p.InboundID)+"/resetClientTraffic/"+url.PathEscape(p.Email), map[string]any{}); err != nil {
			if err.Error() == "xui_request_unknown" {
				return failure("xui_reset_unknown")
			}
			return failure(err.Error())
		}
	case "xui.client.read":
	default:
		return failure("xui_payload_invalid")
	}
	// SS2022 hot updates can report success while leaving the user unusable.
	// Even a recovery finding identical config must reload it. A journal terminal
	// replay does not reach this code. No new generic shell/restart command is exposed.
	if h.kind != "xui.client.read" && p.Protocol == "shadowsocks" && strings.HasPrefix(p.Method, "2022-") {
		if err = h.adapter.restart(ctx); err != nil {
			return failure("xui_restart_unknown")
		}
	}
	_, after, err := h.adapter.get(ctx, p.InboundID)
	if err != nil {
		return failure(err.Error())
	}
	c, err = owned(after, p)
	if err != nil {
		return failure(err.Error())
	}
	if h.kind == "xui.client.ensure" && (c == nil || c["enable"] != effectiveEnable || number(c["totalGB"]) != p.TotalBytes || number(c["tgId"]) != p.TgID || number(c["expiryTime"]) != 0) {
		return failure("xui_target_unconfirmed")
	}
	if h.kind == "xui.client.delete" && c != nil && (!retained || c["enable"] != false) {
		return failure("xui_target_unconfirmed")
	}
	observation := map[string]any{"present": c != nil, "enable": false, "traffic_enable": false, "total_bytes": int64(0), "up": int64(0), "down": int64(0), "tg_id": int64(0), "checked_at": time.Now().UTC().Format(time.RFC3339Nano)}
	if c != nil {
		t, err := h.adapter.traffic(ctx, p.Email)
		if err != nil {
			return failure(err.Error())
		}
		observation["enable"] = c["enable"] == true
		observation["traffic_enable"] = t["enable"] == true
		observation["total_bytes"] = number(c["totalGB"])
		observation["up"] = number(t["up"])
		observation["down"] = number(t["down"])
		observation["tg_id"] = number(c["tgId"])
	}
	result := map[string]any{"observation": observation}
	if h.kind == "xui.client.delete" {
		result["retained_disabled"] = retained
	}
	if h.kind == "xui.client.read" {
		links := []string{}
		code := ""
		if c != nil {
			links, err = h.adapter.links(ctx, p)
			if err != nil {
				links = []string{}
				code = err.Error()
			}
		}
		result["links"] = links
		result["links_error"] = code
	}
	return success("xui_ok", result)
}
func number(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}
