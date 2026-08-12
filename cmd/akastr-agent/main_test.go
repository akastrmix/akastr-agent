package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckConfigValidatesRuntimeDependencies(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	config := `{
  "schema_version": 1,
  "node": {"id": "123e4567-e89b-42d3-a456-426614174000", "name": "test-node"},
  "control": {
    "endpoint": "wss://control.example/internal/agents/ws",
    "credential_file": "/tmp/akastr-agent-test-identity.json",
    "enrollment_token_file": "/tmp/akastr-agent-test-token"
  },
  "state_file": "/tmp/akastr-agent-test-state.json",
  "ip_state_file": "/tmp/akastr-agent-test-ip-state.json",
  "recent_operation_limit": 16,
  "capabilities": {
    "ip_watch": {"enabled": false},
    "change_ip": {
      "enabled": true,
      "program": "/definitely/missing/akastr-changeip",
      "args": ["change"],
      "timeout_seconds": 30,
      "observe_timeout_seconds": 60
    },
    "socks5": {"enabled": false},
    "ipquality_runner": {"enabled": false}
  }
}`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	err := run([]string{"check-config", "--config", configPath}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "stat ChangeIP program") {
		t.Fatalf("check-config error = %v, want missing ChangeIP program", err)
	}
}
