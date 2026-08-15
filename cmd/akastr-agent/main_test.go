package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckIdentityAcceptsOnlyTheExpectedPersistentNode(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(t.TempDir(), "identity.json")
	contents, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"agent_id":       "123e4567-e89b-42d3-a456-426614174000",
		"public_key":     base64.RawURLEncoding.EncodeToString(publicKey),
		"private_key":    base64.RawURLEncoding.EncodeToString(privateKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identityPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{
		"check-identity", "--identity", identityPath,
		"--agent-id", "123e4567-e89b-42d3-a456-426614174000",
	}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{
		"check-identity", "--identity", identityPath,
		"--agent-id", "123e4567-e89b-42d3-a456-426614174001",
	}, &bytes.Buffer{}); err == nil {
		t.Fatal("identity for another persistent node was accepted")
	}
}

func TestCheckConfigValidatesRuntimeDependencies(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	config := `{
  "schema_version": 2,
  "node": {"id": "123e4567-e89b-42d3-a456-426614174000", "name": "test-node"},
  "control": {
    "endpoint": "wss://control.example/internal/agents/ws",
    "credential_file": "/tmp/akastr-agent-test-identity.json",
    "machine_token_file": "/tmp/akastr-agent-test-token"
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
