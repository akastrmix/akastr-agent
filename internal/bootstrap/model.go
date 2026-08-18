package bootstrap

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/akastrmix/akastr-agent/internal/config"
)

const SchemaVersion = 2

const (
	IPQualityCommit  = "0ee5f192fed70c04615852efba0e4b8bd43546c7"
	IPQualityVersion = "0ee5f192fed7"
	IPQualitySHA256  = "9823c560e0d19769eb627329a31cb47da655d087166d86e40d9b6c77bc7f32fb"
)

const (
	identityPath = "/etc/akastr-agent/identity.json"
	tokenPath    = "/etc/akastr-agent/machine-token"
	statePath    = "/var/lib/akastr-agent/state.json"
	ipStatePath  = "/var/lib/akastr-agent/ip-state.json"
	curlPath     = "/etc/akastr-agent/changeip-curl.conf"
	profilesPath = "/etc/akastr-agent/proxy-profiles.json"
	scriptPath   = "/usr/local/lib/akastr-agent/ipquality/ip.sh"
)

var (
	canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	stableID      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	bearerToken   = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
)

type Payload struct {
	SchemaVersion   int     `json:"schema_version"`
	Mode            string  `json:"mode"`
	AgentID         string  `json:"agent_id"`
	Name            string  `json:"name"`
	ControlEndpoint string  `json:"control_endpoint"`
	Target          *Target `json:"target,omitempty"`
	Runner          *Runner `json:"runner,omitempty"`
}

type Target struct {
	IPWatchIntervalSeconds int      `json:"ip_watch_interval_seconds"`
	ChangeIP               ChangeIP `json:"change_ip"`
	SOCKS5                 SOCKS5   `json:"socks5"`
}

type ChangeIP struct {
	Provider    string   `json:"provider"`
	URL         string   `json:"url,omitempty"`
	BearerToken string   `json:"bearer_token,omitempty"`
	Program     string   `json:"program,omitempty"`
	Args        []string `json:"args,omitempty"`
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

func (p Payload) Validate(expectedAgentID string) error {
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("bootstrap schema_version must be %d", SchemaVersion)
	}
	if !canonicalUUID.MatchString(p.AgentID) || p.AgentID != expectedAgentID {
		return errors.New("bootstrap agent_id is invalid or mismatched")
	}
	if name := strings.TrimSpace(p.Name); name == "" || name != p.Name || len(name) > 64 {
		return errors.New("bootstrap name must be 1-64 characters without surrounding whitespace")
	}
	endpoint, err := url.Parse(p.ControlEndpoint)
	if err != nil || endpoint.Scheme != "wss" || endpoint.Host == "" || endpoint.Path != "/internal/agents/ws" || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.User != nil {
		return errors.New("bootstrap control_endpoint must be an absolute WSS control URL")
	}
	switch p.Mode {
	case "target":
		if p.Target == nil || p.Runner != nil {
			return errors.New("target bootstrap must contain only target configuration")
		}
		return p.Target.validate()
	case "runner":
		if p.Runner == nil || p.Target != nil {
			return errors.New("runner bootstrap must contain only runner configuration")
		}
		return p.Runner.validate()
	default:
		return errors.New("bootstrap mode must be target or runner")
	}
}

func (t Target) validate() error {
	if t.IPWatchIntervalSeconds < 10 || t.IPWatchIntervalSeconds > 300 {
		return errors.New("target IP watch interval must be between 10 and 300 seconds")
	}
	if err := t.ChangeIP.validate(); err != nil {
		return err
	}
	return t.SOCKS5.validate()
}

func (c ChangeIP) validate() error {
	switch c.Provider {
	case "disabled":
		if c.URL != "" || c.BearerToken != "" || c.Program != "" || len(c.Args) != 0 {
			return errors.New("disabled ChangeIP provider must not contain configuration")
		}
	case "http_bearer":
		parsed, err := url.Parse(c.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || len(c.URL) > 2048 || strings.ContainsAny(c.URL, "\r\n") {
			return errors.New("ChangeIP URL must be a bounded absolute HTTPS URL")
		}
		if len(c.BearerToken) > 4096 || !bearerToken.MatchString(c.BearerToken) || c.Program != "" || len(c.Args) != 0 {
			return errors.New("HTTP ChangeIP provider configuration is invalid")
		}
	case "command":
		if !path.IsAbs(c.Program) || path.Clean(c.Program) != c.Program || len(c.Program) > 4096 || strings.ContainsRune(c.Program, '\x00') {
			return errors.New("ChangeIP program must be a clean absolute path")
		}
		switch c.Program {
		case "/bin/sh", "/bin/bash", "/usr/bin/env":
			return errors.New("generic shell ChangeIP programs are forbidden")
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

func (s SOCKS5) validate() error {
	if !s.Enabled {
		if s.Port != 0 {
			return errors.New("disabled SOCKS5 description must not contain configuration")
		}
		return nil
	}
	if s.Port < 1 || s.Port > 65535 {
		return errors.New("SOCKS5 port must be between 1 and 65535")
	}
	return nil
}

func (r Runner) validate() error {
	if len(r.Profiles) < 1 || len(r.Profiles) > 128 {
		return errors.New("runner must contain 1-128 proxy profiles")
	}
	seen := map[string]struct{}{}
	for _, profile := range r.Profiles {
		if !stableID.MatchString(profile.ID) || profile.Username == "" || profile.Password == "" || len(profile.Username) > 255 || len(profile.Password) > 255 || strings.ContainsRune(profile.Username+profile.Password, '\x00') {
			return fmt.Errorf("runner proxy profile %q is invalid", profile.ID)
		}
		if _, exists := seen[profile.ID]; exists {
			return fmt.Errorf("runner proxy profile %q is repeated", profile.ID)
		}
		seen[profile.ID] = struct{}{}
	}
	return nil
}

func (p Payload) AgentConfig(ipQualityVersion, ipQualitySHA256 string) config.Config {
	cfg := config.Config{
		SchemaVersion: SchemaVersion,
		Node:          config.NodeConfig{ID: p.AgentID, Name: p.Name},
		Control:       config.ControlConfig{Endpoint: p.ControlEndpoint, CredentialFile: identityPath, MachineTokenFile: tokenPath},
		StateFile:     statePath, IPStateFile: ipStatePath, RecentOperationLimit: 64,
	}
	if p.Mode == "target" {
		cfg.Capabilities.IPWatch = config.IPWatchConfig{Enabled: true, IntervalSeconds: p.Target.IPWatchIntervalSeconds, ObserveIPv6: false}
		switch p.Target.ChangeIP.Provider {
		case "http_bearer":
			cfg.Capabilities.ChangeIP = config.ChangeIPConfig{Enabled: true, Program: "/usr/bin/curl", Args: []string{"--config", curlPath}, TimeoutSeconds: 60, ObserveTimeoutSeconds: 300}
		case "command":
			cfg.Capabilities.ChangeIP = config.ChangeIPConfig{Enabled: true, Program: p.Target.ChangeIP.Program, Args: p.Target.ChangeIP.Args, TimeoutSeconds: 60, ObserveTimeoutSeconds: 300}
		}
		cfg.Capabilities.SOCKS5 = config.SOCKS5Config{Enabled: p.Target.SOCKS5.Enabled, Port: p.Target.SOCKS5.Port}
	} else {
		cfg.Capabilities.IPQualityRunner = config.IPQualityRunnerConfig{Enabled: true, ScriptPath: scriptPath, ProxyProfilesFile: profilesPath, TimeoutSeconds: 900, MaxConcurrency: 1, ScriptVersion: ipQualityVersion, ScriptSHA256: ipQualitySHA256}
	}
	return cfg
}
