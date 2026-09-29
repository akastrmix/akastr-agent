package ipqualityrunner

import (
	"encoding/json"
	"errors"
	"regexp"

	"github.com/akastrmix/akastr-agent/internal/capability"
	qualityscript "github.com/akastrmix/akastr-agent/internal/modules/ipqualityrunner/script"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

// Module name in the node configuration.
const (
	Name        = "ipquality_runner"
	CommandType = "ipquality.execute"
)

var stableID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Config has no settings: each operation carries the target's SOCKS5 login,
// so the Runner keeps no proxy credentials and needs no change when targets do.
type Config struct{}

func ParseConfig(raw json.RawMessage) (Config, error) {
	return protocol.DecodeStrict[Config](raw, Name)
}

func (Config) Capability() capability.Descriptor {
	return capability.Descriptor{
		Name: "ipquality.runner", Version: 2, ExclusiveGroups: []string{"ipquality-runner"},
		Properties: map[string]any{"max_concurrency": "1", "script_version": qualityscript.PinnedVersion},
	}
}

type Payload struct {
	ExpectedIPv4  string `json:"expected_ipv4"`
	ProxyPort     int    `json:"proxy_port"`
	ProxyUsername string `json:"proxy_username"`
	ProxyPassword string `json:"proxy_password"`
	ScriptVersion string `json:"script_version"`
}

func DecodePayload(raw json.RawMessage) (Payload, error) {
	payload, err := protocol.DecodeStrict[Payload](raw, CommandType+" payload",
		"expected_ipv4", "proxy_port", "proxy_username", "proxy_password", "script_version")
	if err != nil {
		return Payload{}, err
	}
	if !protocol.PublicIPv4(payload.ExpectedIPv4) || payload.ProxyPort < 1 || payload.ProxyPort > 65535 ||
		!protocol.SOCKSCredential(payload.ProxyUsername) || !protocol.SOCKSCredential(payload.ProxyPassword) ||
		!stableID.MatchString(payload.ScriptVersion) {
		return Payload{}, errors.New("ipquality.execute payload is invalid")
	}
	return payload, nil
}
