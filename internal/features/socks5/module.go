// Package socks5 describes an existing SOCKS5 proxy on a target node. The
// Agent neither runs nor configures the proxy; Cloud pairs the port with the
// node's observed public IPv4 and hands the login to the IPQuality Runner with
// each check.
package socks5

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/akastrmix/akastr-agent/internal/capability"
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
	if !protocol.SOCKSCredential(cfg.Username) || !protocol.SOCKSCredential(cfg.Password) {
		return Config{}, errors.New("SOCKS5 username and password must be 1-255 bytes without NUL")
	}
	return cfg, nil
}

// Capability advertises the port only; Cloud already holds the login.
func (c Config) Capability() capability.Descriptor {
	return capability.Descriptor{
		Name: "proxy.socks5", Version: 1, Properties: map[string]any{"port": strconv.Itoa(c.Port)},
	}
}
