// Package socks5 describes an existing SOCKS5 proxy on a target node. The
// Agent neither runs nor configures the proxy; it only accepts the section of
// its configuration. Cloud pairs the port with the node's observed public IPv4
// and hands the login to the IPQuality Runner with each check.
package socks5

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const Name = "socks5"

type Config struct {
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func ParseConfig(raw json.RawMessage) (Config, error) {
	cfg, err := protocol.DecodeStrict[Config](raw, Name, "port", "username", "password")
	if err != nil {
		return Config{}, err
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return Config{}, errors.New("SOCKS5 port must be between 1 and 65535")
	}
	if !validLogin(cfg.Username) || !validLogin(cfg.Password) {
		return Config{}, errors.New("SOCKS5 username and password must be 1-255 bytes without NUL")
	}
	return cfg, nil
}

// validLogin reports whether value is a usable SOCKS5 username or password
// (RFC 1929 allows up to 255 bytes).
func validLogin(value string) bool {
	return value != "" && len(value) <= 255 && !strings.ContainsRune(value, '\x00')
}
