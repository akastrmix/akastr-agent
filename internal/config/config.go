package config

import (
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

const SchemaVersion = 3

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var changeIPCommandInterpreters = map[string]struct{}{
	"bash": {}, "busybox": {}, "dash": {}, "env": {}, "sh": {},
}

var changeIPCommandHiddenRoots = []string{"/home", "/root", "/run/user", "/tmp", "/var/tmp"}

var reconciledSecretPath = regexp.MustCompile(`^/var/lib/akastr-agent/configurations/[1-9][0-9]*/(changeip-curl\.conf|proxy-profiles\.json)$`)

type Config struct {
	SchemaVersion         int                `json:"schema_version"`
	ConfigurationRevision int64              `json:"configuration_revision"`
	Node                  NodeConfig         `json:"node"`
	Control               ControlConfig      `json:"control"`
	StateFile             string             `json:"state_file"`
	IPStateFile           string             `json:"ip_state_file"`
	RecentOperationLimit  int                `json:"recent_operation_limit"`
	Capabilities          CapabilitiesConfig `json:"capabilities"`
}

type NodeConfig struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ControlConfig struct {
	Endpoint         string `json:"endpoint"`
	CredentialFile   string `json:"credential_file"`
	MachineTokenFile string `json:"machine_token_file"`
}

type CapabilitiesConfig struct {
	IPWatch         IPWatchConfig         `json:"ip_watch"`
	ChangeIP        ChangeIPConfig        `json:"change_ip"`
	SOCKS5          SOCKS5Config          `json:"socks5"`
	IPQualityRunner IPQualityRunnerConfig `json:"ipquality_runner"`
}

type IPWatchConfig struct {
	Enabled         bool `json:"enabled"`
	IntervalSeconds int  `json:"interval_seconds"`
	ObserveIPv6     bool `json:"observe_ipv6"`
}

type ChangeIPConfig struct {
	Provider              string   `json:"provider"`
	Program               string   `json:"program"`
	Args                  []string `json:"args"`
	TimeoutSeconds        int      `json:"timeout_seconds"`
	ObserveTimeoutSeconds int      `json:"observe_timeout_seconds"`
}

type SOCKS5Config struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port"`
}

type IPQualityRunnerConfig struct {
	Enabled           bool   `json:"enabled"`
	ScriptPath        string `json:"script_path"`
	ProxyProfilesFile string `json:"proxy_profiles_file"`
	TimeoutSeconds    int    `json:"timeout_seconds"`
	MaxConcurrency    int    `json:"max_concurrency"`
	ScriptVersion     string `json:"script_version"`
	ScriptSHA256      string `json:"script_sha256"`
}

func Load(filePath string) (Config, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("decode config: multiple JSON values")
		}
		return Config{}, fmt.Errorf("decode config trailing data: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %d", SchemaVersion)
	}
	if c.ConfigurationRevision < 1 {
		return errors.New("configuration_revision must be a positive integer")
	}
	if !canonicalUUID.MatchString(c.Node.ID) || c.Node.ID == "00000000-0000-0000-0000-000000000000" {
		return errors.New("node.id must be a non-zero canonical lowercase UUID")
	}
	if name := strings.TrimSpace(c.Node.Name); name == "" || name != c.Node.Name || len(name) > 64 {
		return errors.New("node.name must be 1-64 characters without surrounding whitespace")
	}
	if err := validateControl(c.Control); err != nil {
		return err
	}
	if err := validateAbsoluteLinuxPath("state_file", c.StateFile); err != nil {
		return err
	}
	if err := validateAbsoluteLinuxPath("ip_state_file", c.IPStateFile); err != nil {
		return err
	}
	if c.RecentOperationLimit < 16 || c.RecentOperationLimit > 1024 {
		return errors.New("recent_operation_limit must be between 16 and 1024")
	}
	if err := validateCapabilities(c.Capabilities); err != nil {
		return err
	}
	return nil
}

func validateControl(control ControlConfig) error {
	endpoint, err := url.Parse(control.Endpoint)
	if err != nil {
		return fmt.Errorf("control.endpoint is invalid: %w", err)
	}
	if endpoint.Scheme != "wss" || endpoint.Host == "" {
		return errors.New("control.endpoint must be an absolute wss URL")
	}
	if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("control.endpoint must not contain user info, query, or fragment")
	}
	if endpoint.Path != "/internal/agents/ws" {
		return errors.New("control.endpoint path must be /internal/agents/ws")
	}
	if err := validateAbsoluteLinuxPath("control.credential_file", control.CredentialFile); err != nil {
		return err
	}
	return validateAbsoluteLinuxPath("control.machine_token_file", control.MachineTokenFile)
}

func validateCapabilities(capabilities CapabilitiesConfig) error {
	enabled := 0
	if capabilities.IPWatch.Enabled {
		enabled++
		if capabilities.IPWatch.IntervalSeconds < 10 || capabilities.IPWatch.IntervalSeconds > 300 {
			return errors.New("capabilities.ip_watch.interval_seconds must be between 10 and 300")
		}
	}
	switch capabilities.ChangeIP.Provider {
	case "disabled":
		if capabilities.ChangeIP.Program != "" || len(capabilities.ChangeIP.Args) != 0 ||
			capabilities.ChangeIP.TimeoutSeconds != 0 || capabilities.ChangeIP.ObserveTimeoutSeconds != 0 {
			return errors.New("disabled ChangeIP provider must not contain configuration")
		}
	case "http_bearer", "command":
		enabled++
		if !capabilities.IPWatch.Enabled {
			return errors.New("capabilities.change_ip requires capabilities.ip_watch")
		}
		if err := validateAbsoluteLinuxPath("capabilities.change_ip.program", capabilities.ChangeIP.Program); err != nil {
			return err
		}
		if capabilities.ChangeIP.Provider == "http_bearer" {
			if capabilities.ChangeIP.Program != "/usr/bin/curl" {
				return errors.New("capabilities.change_ip HTTP provider program is invalid")
			}
			if len(capabilities.ChangeIP.Args) != 2 || capabilities.ChangeIP.Args[0] != "--config" ||
				(capabilities.ChangeIP.Args[1] != "/etc/akastr-agent/changeip-curl.conf" &&
					!reconciledSecretPath.MatchString(capabilities.ChangeIP.Args[1])) {
				return errors.New("capabilities.change_ip curl provider configuration is invalid")
			}
		} else if err := ValidateChangeIPCommandProgram(capabilities.ChangeIP.Program); err != nil {
			return err
		}
		if len(capabilities.ChangeIP.Args) > 32 {
			return errors.New("capabilities.change_ip.args must contain at most 32 arguments")
		}
		for index, argument := range capabilities.ChangeIP.Args {
			if argument == "" || strings.ContainsRune(argument, '\x00') {
				return fmt.Errorf("capabilities.change_ip.args[%d] must be non-empty and contain no NUL", index)
			}
		}
		if capabilities.ChangeIP.TimeoutSeconds < 1 || capabilities.ChangeIP.TimeoutSeconds > 300 {
			return errors.New("capabilities.change_ip.timeout_seconds must be between 1 and 300")
		}
		if capabilities.ChangeIP.ObserveTimeoutSeconds < 30 || capabilities.ChangeIP.ObserveTimeoutSeconds > 900 {
			return errors.New("capabilities.change_ip.observe_timeout_seconds must be between 30 and 900")
		}
	default:
		return errors.New("capabilities.change_ip.provider must be disabled, http_bearer, or command")
	}
	if capabilities.SOCKS5.Enabled {
		enabled++
		if capabilities.SOCKS5.Port < 1 || capabilities.SOCKS5.Port > 65535 {
			return errors.New("capabilities.socks5.port must be between 1 and 65535")
		}
	}
	if capabilities.IPQualityRunner.Enabled {
		enabled++
		if err := validateAbsoluteLinuxPath("capabilities.ipquality_runner.script_path", capabilities.IPQualityRunner.ScriptPath); err != nil {
			return err
		}
		if err := validateAbsoluteLinuxPath("capabilities.ipquality_runner.proxy_profiles_file", capabilities.IPQualityRunner.ProxyProfilesFile); err != nil {
			return err
		}
		if capabilities.IPQualityRunner.TimeoutSeconds < 60 || capabilities.IPQualityRunner.TimeoutSeconds > 1800 {
			return errors.New("capabilities.ipquality_runner.timeout_seconds must be between 60 and 1800")
		}
		if capabilities.IPQualityRunner.MaxConcurrency != 1 {
			return errors.New("capabilities.ipquality_runner.max_concurrency must be 1")
		}
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`).MatchString(capabilities.IPQualityRunner.ScriptVersion) {
			return errors.New("capabilities.ipquality_runner.script_version must be a stable lowercase token")
		}
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(capabilities.IPQualityRunner.ScriptSHA256) {
			return errors.New("capabilities.ipquality_runner.script_sha256 must be a lowercase SHA-256 digest")
		}
	}
	if enabled == 0 {
		return errors.New("at least one capability must be enabled")
	}
	return nil
}

func ValidateChangeIPCommandProgram(value string) error {
	if err := validateAbsoluteLinuxPath("ChangeIP command program", value); err != nil {
		return err
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

func validateAbsoluteLinuxPath(field, value string) error {
	if value == "" || !path.IsAbs(value) || path.Clean(value) != value || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s must be a clean absolute Linux path", field)
	}
	return nil
}
