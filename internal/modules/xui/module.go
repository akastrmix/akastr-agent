// Package xui manages only Cloud-owned clients in existing local 3x-ui inbounds.
package xui

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const Name = "xui"

var CommandTypes = []string{"xui.inbounds.list", "xui.client.ensure", "xui.client.delete", "xui.client.read", "xui.client.reset_traffic"}
var identityText = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type Config struct {
	PanelURL         string `json:"panel_url"`
	Username         string `json:"username"`
	Password         string `json:"password"`
	SubscriptionURL  string `json:"subscription_url"`
	SubscriptionHost string `json:"subscription_host"`
}

func ParseConfig(raw json.RawMessage) (Config, error) {
	c, err := protocol.DecodeStrict[Config](raw, Name, "panel_url", "username", "password", "subscription_url", "subscription_host")
	if err != nil {
		return c, err
	}
	for _, value := range []string{c.PanelURL, c.SubscriptionURL} {
		u, err := url.Parse(value)
		if err != nil || len(value) > 2048 || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return c, errors.New("xui URL must be an absolute loopback HTTP URL")
		}
		ip, err := netip.ParseAddr(u.Hostname())
		if err != nil || !ip.IsLoopback() {
			return c, errors.New("xui URL requires a literal loopback address")
		}
	}
	if c.Username == "" || len(c.Username) > 128 || c.Password == "" || len(c.Password) > 1024 || strings.ContainsAny(c.Username+c.Password, "\x00\r\n") {
		return c, errors.New("xui login is invalid")
	}
	u, err := url.Parse("http://" + c.SubscriptionHost)
	if err != nil || c.SubscriptionHost == "" || len(c.SubscriptionHost) > 253 || u.User != nil || u.Hostname() == "" || u.Port() != "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(c.SubscriptionHost, "\r\n\t /\\") {
		return c, errors.New("xui subscription host must be a domain or IP, without a port")
	}
	return c, nil
}

type Payload struct {
	InboundID  int    `json:"inbound_id"`
	Protocol   string `json:"protocol"`
	Method     string `json:"method"`
	Email      string `json:"email"`
	SubID      string `json:"sub_id"`
	Credential string `json:"credential"`
	Flow       string `json:"flow"`
	Enable     bool   `json:"enable,omitempty"`
	TotalBytes int64  `json:"total_bytes,omitempty"`
	TgID       int64  `json:"tg_id,omitempty"`
}

func decodePayload(kind string, raw json.RawMessage) (Payload, error) {
	fields := []string{"inbound_id", "protocol", "method", "email", "sub_id", "credential", "flow"}
	if kind == "xui.client.ensure" {
		fields = append(fields, "enable", "total_bytes", "tg_id")
	}
	p, err := protocol.DecodeStrict[Payload](raw, kind, fields...)
	if err != nil {
		return p, err
	}
	if p.InboundID < 1 || !identityText.MatchString(p.Email) || !identityText.MatchString(p.SubID) || p.TotalBytes < 0 || p.TotalBytes > 9007199254740991 || p.TgID < 0 || p.TgID > 9007199254740991 {
		return p, errors.New("xui client identity or limit is invalid")
	}
	if p.Flow != "" && p.Flow != "xtls-rprx-vision" {
		return p, errors.New("xui flow is invalid")
	}
	switch p.Protocol {
	case "vmess", "vless":
		if !protocol.ValidUUID(p.Credential) || p.Method != "" {
			return p, errors.New("xui UUID credential is invalid")
		}
	case "shadowsocks":
		n := 32
		if p.Method == "2022-blake3-aes-128-gcm" {
			n = 16
		} else if p.Method != "2022-blake3-aes-256-gcm" && p.Method != "2022-blake3-chacha20-poly1305" && p.Method != "chacha20-ietf-poly1305" && p.Method != "aes-128-gcm" && p.Method != "aes-256-gcm" {
			return p, errors.New("xui Shadowsocks method is unsupported")
		}
		if strings.HasPrefix(p.Method, "2022-") {
			b, e := base64.StdEncoding.DecodeString(p.Credential)
			if e != nil || len(b) != n {
				return p, errors.New("xui Shadowsocks key size is invalid")
			}
		}
	case "trojan", "hysteria", "hysteria2":
		if p.Method != "" {
			return p, errors.New("xui method is invalid")
		}
	default:
		return p, errors.New("xui protocol is unsupported")
	}
	if len(p.Credential) < 16 || len(p.Credential) > 256 || strings.ContainsAny(p.Credential, "\x00\r\n") {
		return p, errors.New("xui credential is invalid")
	}
	return p, nil
}

func credentialField(proto string) string {
	switch proto {
	case "trojan", "shadowsocks":
		return "password"
	case "hysteria", "hysteria2":
		return "auth"
	default:
		return "id"
	}
}
