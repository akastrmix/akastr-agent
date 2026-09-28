package changeip

import (
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

// Module name in the node configuration. It requires the ip_watch module,
// which confirms every trigger through the observed public IPv4.
const (
	Name        = "changeip"
	CommandType = "changeip.execute"
)

var bearerToken = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

var shellEntryPoints = map[string]struct{}{"bash": {}, "busybox": {}, "dash": {}, "env": {}, "sh": {}}

// Directories hidden from the service by its systemd sandbox.
var sandboxHiddenRoots = []string{"/home", "/root", "/run/user", "/tmp", "/var/tmp"}

type Config struct {
	Provider string `json:"provider"`
	// SourceCommand is the administrator's edit text, kept by Cloud for
	// round-trips only. The Agent never executes it.
	SourceCommand string   `json:"source_command,omitempty"`
	URL           string   `json:"url,omitempty"`
	BearerToken   string   `json:"bearer_token,omitempty"`
	Program       string   `json:"program,omitempty"`
	Args          []string `json:"args,omitempty"`
}

func ParseConfig(raw json.RawMessage) (Config, error) {
	cfg, err := protocol.DecodeKnown[Config](raw, Name)
	if err != nil {
		return Config{}, err
	}
	switch cfg.Provider {
	case "http_bearer":
		parsed, parseErr := url.Parse(cfg.URL)
		if parseErr != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
			parsed.Fragment != "" || len(cfg.URL) > 2048 || strings.ContainsAny(cfg.URL, "\r\n") {
			return Config{}, errors.New("ChangeIP URL must be a bounded absolute HTTPS URL")
		}
		if len(cfg.BearerToken) > 4096 || !bearerToken.MatchString(cfg.BearerToken) ||
			len(cfg.SourceCommand) > 8192 || strings.ContainsRune(cfg.SourceCommand, '\x00') ||
			cfg.Program != "" || cfg.Args != nil {
			return Config{}, errors.New("HTTP ChangeIP provider configuration is invalid")
		}
	case "command":
		if err := validateProgram(cfg.Program); err != nil {
			return Config{}, err
		}
		if cfg.URL != "" || cfg.BearerToken != "" || cfg.SourceCommand != "" || len(cfg.Args) > 32 {
			return Config{}, errors.New("command ChangeIP provider configuration is invalid")
		}
		for _, argument := range cfg.Args {
			if argument == "" || len(argument) > 4096 || strings.ContainsRune(argument, '\x00') {
				return Config{}, errors.New("ChangeIP arguments must be non-empty, bounded, and contain no NUL")
			}
		}
	default:
		return Config{}, errors.New("ChangeIP provider must be http_bearer or command")
	}
	return cfg, nil
}

// validateProgram rejects programs that would reopen a generic shell entry
// point or that the service sandbox cannot see.
func validateProgram(value string) error {
	if value == "" || len(value) > 4096 || !path.IsAbs(value) || path.Clean(value) != value ||
		strings.IndexFunc(value, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
		return errors.New("ChangeIP program must be a clean absolute path without control characters")
	}
	if _, denied := shellEntryPoints[path.Base(value)]; denied {
		return errors.New("ChangeIP program must not be a generic shell entry point")
	}
	for _, root := range sandboxHiddenRoots {
		if value == root || strings.HasPrefix(value, root+"/") {
			return errors.New("ChangeIP program must be visible to the Agent service sandbox")
		}
	}
	return nil
}

func (Config) Capability() capability.Descriptor {
	return capability.Descriptor{Name: "changeip.command", Version: 1, ExclusiveGroups: []string{"target-network"}}
}

type Payload struct {
	ExpectedIPv4 string `json:"expected_ipv4"`
}

func DecodePayload(raw json.RawMessage) (Payload, error) {
	payload, err := protocol.DecodeStrict[Payload](raw, CommandType+" payload", "expected_ipv4")
	if err != nil {
		return Payload{}, err
	}
	if !protocol.PublicIPv4(payload.ExpectedIPv4) {
		return Payload{}, errors.New("changeip.execute expected IPv4 is invalid")
	}
	return payload, nil
}
