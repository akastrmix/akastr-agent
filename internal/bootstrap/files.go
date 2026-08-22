package bootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/akastrmix/akastr-agent/internal/config"
)

const ConfigurationBootstrapDigestFile = ".bootstrap-sha256"

type proxyProfileFile struct {
	SchemaVersion int                     `json:"schema_version"`
	Profiles      map[string]proxyProfile `json:"profiles"`
}

type proxyProfile struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func writeFiles(directory, runtimeDirectory string, payload Payload, token, ipQualityVersion, ipQualitySHA256 string) error {
	if err := prepareEmptyRootOnlyDirectory(directory); err != nil {
		return err
	}
	cfg := payload.AgentConfig(ipQualityVersion, ipQualitySHA256)
	if runtimeDirectory != "" {
		cfg = payload.AgentConfigForDirectory(filepath.ToSlash(runtimeDirectory), ipQualityVersion, ipQualitySHA256)
	}
	if err := writeRuntimeFiles(directory, payload, cfg); err != nil {
		return err
	}
	if err := writeFileSynced(filepath.Join(directory, "machine-token"), []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("write machine token: %w", err)
	}
	return syncDirectory(directory)
}

func MaterializeConfiguration(directory, runtimeDirectory string, raw []byte, expectedAgentID string, expectedRevision int64) (Payload, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var payload Payload
	if err := decoder.Decode(&payload); err != nil {
		return Payload{}, fmt.Errorf("decode configuration bootstrap: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Payload{}, errors.New("configuration bootstrap contains trailing JSON")
	}
	if err := payload.Validate(expectedAgentID); err != nil {
		return Payload{}, err
	}
	if payload.ConfigurationRevision != expectedRevision {
		return Payload{}, errors.New("configuration bootstrap revision is mismatched")
	}
	if err := prepareEmptyRootOnlyDirectory(directory); err != nil {
		return Payload{}, err
	}
	cfg := payload.AgentConfigForDirectory(runtimeDirectory, IPQualityVersion, IPQualitySHA256)
	if err := writeRuntimeFiles(directory, payload, cfg); err != nil {
		return Payload{}, err
	}
	digest := sha256.Sum256(raw)
	if err := writeFileSynced(filepath.Join(directory, ConfigurationBootstrapDigestFile), []byte(fmt.Sprintf("%x\n", digest)), 0o600); err != nil {
		return Payload{}, fmt.Errorf("write configuration bootstrap digest: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return Payload{}, fmt.Errorf("sync configuration directory: %w", err)
	}
	return payload, nil
}

func prepareEmptyRootOnlyDirectory(directory string) error {
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
	return nil
}

func writeRuntimeFiles(directory string, payload Payload, cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("generated Agent config: %w", err)
	}
	if err := writeJSON(filepath.Join(directory, "config.json"), cfg); err != nil {
		return err
	}
	if payload.Mode == "target" && payload.Target.ChangeIP.Provider == "http_bearer" {
		contents := fmt.Sprintf("url = \"%s\"\nrequest = \"POST\"\nheader = \"Authorization: Bearer %s\"\nfail\nsilent\nshow-error\n", escapeCurl(payload.Target.ChangeIP.URL), payload.Target.ChangeIP.BearerToken)
		if err := writeFileSynced(filepath.Join(directory, "changeip-curl.conf"), []byte(contents), 0o600); err != nil {
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
	if err := writeFileSynced(filePath, encoded, 0o600); err != nil {
		return fmt.Errorf("write bootstrap file: %w", err)
	}
	return nil
}

func writeFileSynced(filePath string, contents []byte, mode os.FileMode) error {
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func escapeCurl(value string) string {
	return strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(value)
}
