package changeip

import (
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

// Module name in the node configuration. It requires the ip_watch module,
// which confirms every trigger through the observed public IPv4.
const (
	Name        = "changeip"
	CommandType = "changeip.execute"
)

var bearerToken = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// Directories hidden from the service by its systemd sandbox.
var sandboxHiddenRoots = []string{"/home", "/root", "/run/user", "/tmp", "/var/tmp"}

type Config struct {
	Provider string `json:"provider"`
	// SourceCommand is the administrator's edit text, kept by Cloud for
	// round-trips only. The Agent never executes it.
	SourceCommand string `json:"source_command,omitempty"`
	URL           string `json:"url,omitempty"`
	BearerToken   string `json:"bearer_token,omitempty"`
	// Program runs without arguments: a configuration can pick a program
	// already on the node but cannot hand it anything to run.
	Program string `json:"program,omitempty"`
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
			cfg.Program != "" {
			return Config{}, errors.New("HTTP ChangeIP provider configuration is invalid")
		}
	case "command":
		if err := validateProgram(cfg.Program); err != nil {
			return Config{}, err
		}
		if cfg.URL != "" || cfg.BearerToken != "" || cfg.SourceCommand != "" {
			return Config{}, errors.New("command ChangeIP provider configuration is invalid")
		}
	default:
		return Config{}, errors.New("ChangeIP provider must be http_bearer or command")
	}
	return cfg, nil
}

// validateProgram rejects programs the service sandbox cannot see: installers
// check the configuration outside the sandbox, so such a program would pass
// there and then keep the service from starting.
func validateProgram(value string) error {
	if value == "" || len(value) > 4096 || !path.IsAbs(value) || path.Clean(value) != value ||
		strings.IndexFunc(value, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
		return errors.New("ChangeIP program must be a clean absolute path without control characters")
	}
	for _, root := range sandboxHiddenRoots {
		if value == root || strings.HasPrefix(value, root+"/") {
			return errors.New("ChangeIP program must be visible to the Agent service sandbox")
		}
	}
	return nil
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
