package ipqualityrunner

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

// Module name in the node configuration.
const (
	Name        = "ipquality_runner"
	CommandType = "ipquality.execute"
)

// Config has no settings: each operation carries the target's SOCKS5 login,
// so the Runner keeps no proxy credentials and needs no change when targets do.
type Config struct{}

func ParseConfig(raw json.RawMessage) (Config, error) {
	return protocol.DecodeStrict[Config](raw, Name)
}

type Payload struct {
	ExpectedIPv4  string `json:"expected_ipv4"`
	ProxyPort     int    `json:"proxy_port"`
	ProxyUsername string `json:"proxy_username"`
	ProxyPassword string `json:"proxy_password"`
}

func DecodePayload(raw json.RawMessage) (Payload, error) {
	payload, err := protocol.DecodeStrict[Payload](raw, CommandType+" payload",
		"expected_ipv4", "proxy_port", "proxy_username", "proxy_password")
	if err != nil {
		return Payload{}, err
	}
	if !protocol.PublicIPv4(payload.ExpectedIPv4) || payload.ProxyPort < 1 || payload.ProxyPort > 65535 ||
		!validLogin(payload.ProxyUsername) || !validLogin(payload.ProxyPassword) {
		return Payload{}, errors.New("ipquality.execute payload is invalid")
	}
	return payload, nil
}

// validLogin reports whether value is a usable SOCKS5 username or password
// (RFC 1929 allows up to 255 bytes).
func validLogin(value string) bool {
	return value != "" && len(value) <= 255 && !strings.ContainsRune(value, '\x00')
}
