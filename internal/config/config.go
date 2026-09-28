// Package config reads the node configuration exactly as AkastrCloud issues it.
// The envelope identifies the node; each enabled module owns one section of
// Modules and validates it itself (see internal/app).
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const (
	SchemaVersion = 5
	maxBytes      = 128 * 1024
)

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type Config struct {
	SchemaVersion         int                        `json:"schema_version"`
	ConfigurationRevision int64                      `json:"configuration_revision"`
	AgentID               string                     `json:"agent_id"`
	Name                  string                     `json:"name"`
	ControlEndpoint       string                     `json:"control_endpoint"`
	Modules               map[string]json.RawMessage `json:"modules"`
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

// Parse strictly decodes the envelope. Module sections are validated by their modules.
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
	if cfg.SchemaVersion != SchemaVersion {
		return Config{}, fmt.Errorf("configuration schema_version must be %d", SchemaVersion)
	}
	if cfg.ConfigurationRevision < 1 {
		return Config{}, errors.New("configuration_revision must be a positive integer")
	}
	if !canonicalUUID.MatchString(cfg.AgentID) {
		return Config{}, errors.New("configuration agent_id is invalid")
	}
	if name := strings.TrimSpace(cfg.Name); name == "" || name != cfg.Name || len(name) > 64 {
		return Config{}, errors.New("configuration name must be 1-64 characters without surrounding whitespace")
	}
	endpoint, err := url.Parse(cfg.ControlEndpoint)
	if err != nil || endpoint.Scheme != "wss" || endpoint.Host == "" || endpoint.Path != "/internal/agents/ws" ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.User != nil {
		return Config{}, errors.New("configuration control_endpoint must be an absolute WSS control URL")
	}
	if len(cfg.Modules) == 0 {
		return Config{}, errors.New("configuration must enable at least one module")
	}
	return cfg, nil
}
