package bootstrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type proxyProfileFile struct {
	SchemaVersion int                     `json:"schema_version"`
	Profiles      map[string]proxyProfile `json:"profiles"`
}

type proxyProfile struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func writeFiles(directory string, payload Payload, token, ipQualityVersion, ipQualitySHA256 string) error {
	info, err := os.Stat(directory)
	if err != nil {
		return fmt.Errorf("stat bootstrap output directory: %w", err)
	}
	if !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return errors.New("bootstrap output directory must be root-only")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read bootstrap output directory: %w", err)
	}
	if len(entries) != 0 {
		return errors.New("bootstrap output directory must be empty")
	}
	cfg := payload.AgentConfig(ipQualityVersion, ipQualitySHA256)
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("generated Agent config: %w", err)
	}
	if err := writeJSON(filepath.Join(directory, "config.json"), cfg); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "enrollment-token"), []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("write enrollment token: %w", err)
	}
	if payload.Mode == "target" && payload.Target.ChangeIP.Provider == "http_bearer" {
		contents := fmt.Sprintf("url = \"%s\"\nrequest = \"POST\"\nheader = \"Authorization: Bearer %s\"\nfail\nsilent\nshow-error\n", escapeCurl(payload.Target.ChangeIP.URL), payload.Target.ChangeIP.BearerToken)
		if err := os.WriteFile(filepath.Join(directory, "changeip-curl.conf"), []byte(contents), 0o600); err != nil {
			return fmt.Errorf("write ChangeIP configuration: %w", err)
		}
	}
	if payload.Mode == "runner" {
		profiles := map[string]proxyProfile{}
		for _, profile := range payload.Runner.Profiles {
			profiles[profile.ID] = proxyProfile{Username: profile.Username, Password: profile.Password}
		}
		if err := writeJSON(filepath.Join(directory, "proxy-profiles.json"), proxyProfileFile{SchemaVersion: 1, Profiles: profiles}); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(filePath string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(filePath, encoded, 0o600); err != nil {
		return fmt.Errorf("write bootstrap file: %w", err)
	}
	return nil
}

func escapeCurl(value string) string {
	return strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(value)
}
