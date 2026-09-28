package ipqualityrunner

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	qualityscript "github.com/akastrmix/akastr-agent/internal/providers/ipquality/script"
)

// Module name in the node configuration.
const (
	Name        = "ipquality_runner"
	CommandType = "ipquality.execute"
)

var stableID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type Profile struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// Config holds one SOCKS5 login per target server, keyed by its server key.
type Config struct {
	Profiles []Profile `json:"profiles"`
}

func ParseConfig(raw json.RawMessage) (Config, error) {
	cfg, err := protocol.DecodeStrict[Config](raw, Name, "profiles")
	if err != nil {
		return Config{}, err
	}
	if len(cfg.Profiles) < 1 || len(cfg.Profiles) > 128 {
		return Config{}, errors.New("runner must contain 1-128 proxy profiles")
	}
	seen := map[string]struct{}{}
	for _, profile := range cfg.Profiles {
		if !stableID.MatchString(profile.ID) || profile.Username == "" || profile.Password == "" ||
			len(profile.Username) > 255 || len(profile.Password) > 255 ||
			strings.ContainsRune(profile.Username+profile.Password, '\x00') {
			return Config{}, fmt.Errorf("runner proxy profile %q is invalid", profile.ID)
		}
		if _, exists := seen[profile.ID]; exists {
			return Config{}, fmt.Errorf("runner proxy profile %q is repeated", profile.ID)
		}
		seen[profile.ID] = struct{}{}
	}
	return cfg, nil
}

// Capability lists profile identifiers only; credentials never leave the node.
func (c Config) Capability() capability.Descriptor {
	ids := make([]string, 0, len(c.Profiles))
	for _, profile := range c.Profiles {
		ids = append(ids, profile.ID)
	}
	sort.Strings(ids)
	return capability.Descriptor{
		Name: "ipquality.runner", Version: 1, ExclusiveGroups: []string{"ipquality-runner"},
		Properties: map[string]any{
			"max_concurrency": "1", "script_version": qualityscript.PinnedVersion, "proxy_profile_ids": ids,
		},
	}
}

func (c Config) ScriptProfiles() map[string]qualityscript.Profile {
	profiles := make(map[string]qualityscript.Profile, len(c.Profiles))
	for _, profile := range c.Profiles {
		profiles[profile.ID] = qualityscript.Profile{Username: profile.Username, Password: profile.Password}
	}
	return profiles
}

type Payload struct {
	ExpectedIPv4   string `json:"expected_ipv4"`
	ProxyPort      int    `json:"proxy_port"`
	ProxyProfileID string `json:"proxy_profile_id"`
	ScriptVersion  string `json:"script_version"`
}

func DecodePayload(raw json.RawMessage) (Payload, error) {
	payload, err := protocol.DecodeStrict[Payload](raw, CommandType+" payload",
		"expected_ipv4", "proxy_port", "proxy_profile_id", "script_version")
	if err != nil {
		return Payload{}, err
	}
	if !protocol.PublicIPv4(payload.ExpectedIPv4) || payload.ProxyPort < 1 || payload.ProxyPort > 65535 ||
		!stableID.MatchString(payload.ProxyProfileID) || !stableID.MatchString(payload.ScriptVersion) {
		return Payload{}, errors.New("ipquality.execute payload is invalid")
	}
	return payload, nil
}
