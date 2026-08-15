package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `{
  "schema_version": 2,
  "node": {"id":"123e4567-e89b-42d3-a456-426614174000","name":"HKT"},
  "control": {"endpoint":"wss://origin.example.com/internal/agents/ws","credential_file":"/etc/akastr-agent/identity.json","machine_token_file":"/etc/akastr-agent/machine-token"},
  "state_file":"/var/lib/akastr-agent/state.json",
  "ip_state_file":"/var/lib/akastr-agent/ip-state.json",
  "recent_operation_limit":64,
  "capabilities": {
    "ip_watch":{"enabled":true,"interval_seconds":60,"observe_ipv6":false},
    "change_ip":{"enabled":false},
    "socks5":{"enabled":false},
    "ipquality_runner":{"enabled":false}
  }
}`

func TestLoadValidConfig(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Node.Name != "HKT" || !cfg.Capabilities.IPWatch.Enabled {
		t.Fatalf("Load() returned unexpected config: %#v", cfg)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	input := strings.Replace(validConfig, `"schema_version": 2`, `"schema_version": 2, "unexpected": true`, 1)
	_, err := Load(writeConfig(t, input))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Load() error = %v, want unknown field", err)
	}
}

func TestLoadRejectsSOCKS5SecretFields(t *testing.T) {
	input := strings.Replace(validConfig, `"socks5":{"enabled":false}`, `"socks5":{"enabled":true,"address_source":"observed_ipv4","port":1080,"password":"secret"}`, 1)
	_, err := Load(writeConfig(t, input))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Load() error = %v, want unknown password field", err)
	}
}

func TestValidateRequiresFixedChangeIPProgram(t *testing.T) {
	input := strings.Replace(validConfig, `"change_ip":{"enabled":false}`, `"change_ip":{"enabled":true,"program":"bash","args":["/root/changeip.sh"]}`, 1)
	_, err := Load(writeConfig(t, input))
	if err == nil || !strings.Contains(err.Error(), "absolute Linux path") {
		t.Fatalf("Load() error = %v, want absolute path error", err)
	}
}

func TestValidateRunnerConcurrencyIsOne(t *testing.T) {
	input := strings.Replace(validConfig, `"ipquality_runner":{"enabled":false}`, `"ipquality_runner":{"enabled":true,"script_path":"/opt/akastr-agent/tools/ipquality/ip.sh","proxy_profiles_file":"/etc/akastr-agent/proxies.json","timeout_seconds":600,"max_concurrency":2,"script_version":"v2026.08.13","script_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, 1)
	_, err := Load(writeConfig(t, input))
	if err == nil || !strings.Contains(err.Error(), "max_concurrency must be 1") {
		t.Fatalf("Load() error = %v, want concurrency error", err)
	}
}

func TestLoadRejectsMultipleJSONValues(t *testing.T) {
	_, err := Load(writeConfig(t, validConfig+"\n{}"))
	if err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("Load() error = %v, want multiple values error", err)
	}
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
