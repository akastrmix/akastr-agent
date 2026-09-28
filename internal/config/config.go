// Package config reads the node configuration exactly as AkastrCloud issues it
// (bootstrap schema 4). The Agent keeps no second, derived configuration format.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
)

const (
	SchemaVersion = 4
	maxBytes      = 128 * 1024
)

var (
	canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	stableID      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	bearerToken   = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
)

var changeIPCommandInterpreters = map[string]struct{}{
	"bash": {}, "busybox": {}, "dash": {}, "env": {}, "sh": {},
}

var changeIPCommandHiddenRoots = []string{"/home", "/root", "/run/user", "/tmp", "/var/tmp"}

type Config struct {
	SchemaVersion         int     `json:"schema_version"`
	ConfigurationRevision int64   `json:"configuration_revision"`
	Mode                  string  `json:"mode"`
	AgentID               string  `json:"agent_id"`
	Name                  string  `json:"name"`
	ControlEndpoint       string  `json:"control_endpoint"`
	Target                *Target `json:"target,omitempty"`
	Runner                *Runner `json:"runner,omitempty"`
}

type Target struct {
	IPWatchIntervalSeconds int      `json:"ip_watch_interval_seconds"`
	ObserveIPv6            *bool    `json:"observe_ipv6,omitempty"`
	ChangeIP               ChangeIP `json:"change_ip"`
	SOCKS5                 SOCKS5   `json:"socks5"`
}

type ChangeIP struct {
	// SourceCommand is the administrator's edit text, kept by Cloud for
	// round-trips only. The Agent never executes it.
	SourceCommand string   `json:"source_command,omitempty"`
	Provider      string   `json:"provider"`
	URL           string   `json:"url,omitempty"`
	BearerToken   string   `json:"bearer_token,omitempty"`
	Program       string   `json:"program,omitempty"`
	Args          []string `json:"args,omitempty"`
}

type SOCKS5 struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port,omitempty"`
}

type Runner struct {
	Profiles []ProxyProfile `json:"profiles"`
}

type ProxyProfile struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// Load reads a root-only configuration file.
func Load(filePath string) (Config, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return Config{}, fmt.Errorf("stat configuration: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return Config{}, errors.New("configuration must be a root-only regular file")
	}
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", err)
	}
	return Parse(raw)
}

// Parse strictly decodes and validates a configuration document.
func Parse(raw []byte) (Config, error) {
	if len(raw) == 0 || len(raw) > maxBytes {
		return Config{}, errors.New("configuration size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("configuration contains trailing JSON")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("configuration schema_version must be %d", SchemaVersion)
	}
	if c.ConfigurationRevision < 1 {
		return errors.New("configuration_revision must be a positive integer")
	}
	if !canonicalUUID.MatchString(c.AgentID) {
		return errors.New("configuration agent_id is invalid")
	}
	if name := strings.TrimSpace(c.Name); name == "" || name != c.Name || len(name) > 64 {
		return errors.New("configuration name must be 1-64 characters without surrounding whitespace")
	}
	endpoint, err := url.Parse(c.ControlEndpoint)
	if err != nil || endpoint.Scheme != "wss" || endpoint.Host == "" || endpoint.Path != "/internal/agents/ws" ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.User != nil {
		return errors.New("configuration control_endpoint must be an absolute WSS control URL")
	}
	switch c.Mode {
	case "target":
		if c.Target == nil || c.Runner != nil {
			return errors.New("target configuration must contain only target settings")
		}
		return c.Target.validate()
	case "runner":
		if c.Runner == nil || c.Target != nil {
			return errors.New("runner configuration must contain only runner settings")
		}
		return c.Runner.validate()
	default:
		return errors.New("configuration mode must be target or runner")
	}
}

func (t Target) validate() error {
	if t.IPWatchIntervalSeconds < 10 || t.IPWatchIntervalSeconds > 300 {
		return errors.New("target IP watch interval must be between 10 and 300 seconds")
	}
	if t.ObserveIPv6 == nil {
		return errors.New("target observe_ipv6 is required")
	}
	if err := t.ChangeIP.validate(); err != nil {
		return err
	}
	if !t.SOCKS5.Enabled {
		if t.SOCKS5.Port != 0 {
			return errors.New("disabled SOCKS5 description must not contain configuration")
		}
	} else if t.SOCKS5.Port < 1 || t.SOCKS5.Port > 65535 {
		return errors.New("SOCKS5 port must be between 1 and 65535")
	}
	return nil
}

func (c ChangeIP) validate() error {
	if len(c.SourceCommand) > 8192 || strings.ContainsRune(c.SourceCommand, '\x00') || (c.Provider != "http_bearer" && c.SourceCommand != "") {
		return errors.New("ChangeIP source command is invalid")
	}
	switch c.Provider {
	case "disabled":
		if c.URL != "" || c.BearerToken != "" || c.Program != "" || len(c.Args) != 0 {
			return errors.New("disabled ChangeIP provider must not contain configuration")
		}
	case "http_bearer":
		parsed, err := url.Parse(c.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" ||
			len(c.URL) > 2048 || strings.ContainsAny(c.URL, "\r\n") {
			return errors.New("ChangeIP URL must be a bounded absolute HTTPS URL")
		}
		if len(c.BearerToken) > 4096 || !bearerToken.MatchString(c.BearerToken) || c.Program != "" || len(c.Args) != 0 {
			return errors.New("HTTP ChangeIP provider configuration is invalid")
		}
	case "command":
		if len(c.Program) > 4096 {
			return errors.New("ChangeIP program path is too long")
		}
		if err := ValidateChangeIPCommandProgram(c.Program); err != nil {
			return err
		}
		if c.URL != "" || c.BearerToken != "" || len(c.Args) > 32 {
			return errors.New("command ChangeIP provider configuration is invalid")
		}
		for _, argument := range c.Args {
			if argument == "" || len(argument) > 4096 || strings.ContainsRune(argument, '\x00') {
				return errors.New("ChangeIP arguments must be non-empty, bounded, and contain no NUL")
			}
		}
	default:
		return errors.New("ChangeIP provider must be disabled, http_bearer, or command")
	}
	return nil
}

func (r Runner) validate() error {
	if len(r.Profiles) < 1 || len(r.Profiles) > 128 {
		return errors.New("runner must contain 1-128 proxy profiles")
	}
	seen := map[string]struct{}{}
	for _, profile := range r.Profiles {
		if !stableID.MatchString(profile.ID) || profile.Username == "" || profile.Password == "" ||
			len(profile.Username) > 255 || len(profile.Password) > 255 ||
			strings.ContainsRune(profile.Username+profile.Password, '\x00') {
			return fmt.Errorf("runner proxy profile %q is invalid", profile.ID)
		}
		if _, exists := seen[profile.ID]; exists {
			return fmt.Errorf("runner proxy profile %q is repeated", profile.ID)
		}
		seen[profile.ID] = struct{}{}
	}
	return nil
}

// ValidateChangeIPCommandProgram rejects programs that would reopen a generic
// shell entry point or that the service sandbox cannot see.
func ValidateChangeIPCommandProgram(value string) error {
	if value == "" || !path.IsAbs(value) || path.Clean(value) != value || strings.ContainsRune(value, '\x00') {
		return errors.New("ChangeIP command program must be a clean absolute Linux path")
	}
	if strings.IndexFunc(value, func(character rune) bool {
		return character < 0x20 || character == 0x7f
	}) >= 0 {
		return errors.New("ChangeIP command program must not contain ASCII control characters")
	}
	if _, denied := changeIPCommandInterpreters[path.Base(value)]; denied {
		return errors.New("ChangeIP command program must not be a generic shell entry point")
	}
	for _, root := range changeIPCommandHiddenRoots {
		if value == root || strings.HasPrefix(value, root+"/") {
			return errors.New("ChangeIP command program must be visible to the Agent service sandbox")
		}
	}
	return nil
}
