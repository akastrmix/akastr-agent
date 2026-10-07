// Package xui keeps the Cloud-owned clients of the local 3x-ui panel in their
// target state and reports the panel's inbounds with those clients' traffic.
// Cloud owns a client exactly when its email starts with ak-; the module never
// creates or edits an inbound and never touches any other client.
package xui

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const (
	Name = "xui"
	// KindSnapshot reports the panel's inbounds and the owned clients' traffic.
	KindSnapshot = "xui.snapshot"
	ownedPrefix  = "ak-"
	visionFlow   = "xtls-rprx-vision"
	maxSafeInt   = 1<<53 - 1
)

var ownedEmail = regexp.MustCompile(`^ak-[a-z0-9]{1,61}$`)

type Config struct {
	PanelURL string `json:"panel_url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func ParseConfig(raw json.RawMessage) (Config, error) {
	c, err := protocol.DecodeStrict[Config](raw, Name, "panel_url", "username", "password")
	if err != nil {
		return c, err
	}
	u, err := url.Parse(c.PanelURL)
	if err != nil || len(c.PanelURL) > 2048 || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return c, errors.New("xui panel_url must be an absolute loopback HTTP URL")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err != nil || !ip.IsLoopback() {
		return c, errors.New("xui panel_url requires a literal loopback address")
	}
	if c.Username == "" || len(c.Username) > 128 || c.Password == "" || len(c.Password) > 1024 || strings.ContainsAny(c.Username+c.Password, "\x00\r\n") {
		return c, errors.New("xui login is invalid")
	}
	return c, nil
}

// Target is what Cloud wants in one inbound, keyed by the inbound's panel id:
// exactly these owned clients. Expiry stays with Cloud, which disables a
// client instead; the panel enforces only the traffic limit.
type Target struct {
	Clients []Client `json:"clients"`
}

type Client struct {
	Email      string `json:"email"`
	Credential string `json:"credential"`
	Flow       string `json:"flow"`
	Enable     bool   `json:"enable"`
	TotalBytes int64  `json:"total_bytes"`
	// ResetSeq rises each time Cloud wants the client's traffic cleared once.
	ResetSeq int64 `json:"reset_seq"`
}

func decodeTarget(key string, raw json.RawMessage) (int, Target, error) {
	id, err := strconv.Atoi(key)
	if err != nil || id < 1 || strconv.Itoa(id) != key {
		return 0, Target{}, errors.New("xui target key must be a panel inbound id")
	}
	target, err := protocol.DecodeStrict[Target](raw, "xui target", "clients")
	if err != nil || target.Clients == nil {
		return 0, Target{}, errors.New("xui target is invalid")
	}
	emails, credentials := map[string]bool{}, map[string]bool{}
	for _, c := range target.Clients {
		if !ownedEmail.MatchString(c.Email) || emails[c.Email] || credentials[c.Credential] ||
			len(c.Credential) < 16 || len(c.Credential) > 256 || strings.ContainsAny(c.Credential, "\x00\r\n\"") ||
			(c.Flow != "" && c.Flow != visionFlow) ||
			c.TotalBytes < 0 || c.TotalBytes > maxSafeInt || c.ResetSeq < 0 || c.ResetSeq > maxSafeInt {
			return 0, Target{}, errors.New("xui target client is invalid")
		}
		emails[c.Email], credentials[c.Credential] = true, true
	}
	return id, target, nil
}

// credentialField names the client field that holds the secret, which the
// panel also uses to address the client, except Shadowsocks (by email).
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

func ss2022(proto, method string) bool {
	return proto == "shadowsocks" && strings.HasPrefix(method, "2022-")
}

// compatible rejects a target whose credentials the inbound cannot use, which
// means the inbound was changed in the panel after Cloud read it.
func compatible(proto, method string, target Target) error {
	for _, c := range target.Clients {
		if c.Flow != "" && proto != "vless" {
			return errors.New("xui_inbound_changed")
		}
		switch {
		case proto == "vless":
			if !protocol.ValidUUID(c.Credential) {
				return errors.New("xui_inbound_changed")
			}
		case ss2022(proto, method):
			size := 32
			if method == "2022-blake3-aes-128-gcm" {
				size = 16
			}
			key, err := base64.StdEncoding.DecodeString(c.Credential)
			if err != nil || len(key) != size {
				return errors.New("xui_inbound_changed")
			}
		case proto == "hysteria" || proto == "hysteria2":
		default:
			return errors.New("xui_inbound_changed")
		}
	}
	return nil
}
