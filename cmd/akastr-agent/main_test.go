package main

import (
	"bytes"
	"encoding/json"
	"github.com/akastrmix/akastr-agent/internal/app"
	agentconfig "github.com/akastrmix/akastr-agent/internal/config"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/akastrmix/akastr-agent/internal/operation"
)

func TestIdleInspectionDoesNotRequireRunnerCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed configuration uses Linux paths")
	}
	root := t.TempDir()
	cfg := agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion, ConfigurationRevision: 1,
		Node:      agentconfig.NodeConfig{ID: "123e4567-e89b-42d3-a456-426614174000", Name: "runner"},
		Control:   agentconfig.ControlConfig{Endpoint: "wss://control.example/internal/agents/ws", CredentialFile: filepath.Join(root, "identity.json"), MachineTokenFile: filepath.Join(root, "token")},
		StateFile: filepath.Join(root, "state.json"), IPStateFile: filepath.Join(root, "ip.json"), RecentOperationLimit: 16,
		Capabilities: agentconfig.CapabilitiesConfig{
			ChangeIP:        agentconfig.ChangeIPConfig{Provider: "disabled"},
			IPQualityRunner: agentconfig.IPQualityRunnerConfig{Enabled: true, ScriptPath: filepath.Join(root, "ip.sh"), ProxyProfilesFile: filepath.Join(root, "missing.json"), TimeoutSeconds: 60, MaxConcurrency: 1, ScriptVersion: "test", ScriptSHA256: strings.Repeat("a", 64)},
		},
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"check-idle", "--config", path}, &bytes.Buffer{}); err != nil {
		t.Fatalf("repair requires missing credentials: %v", err)
	}
	if err := run([]string{"check-config", "--config", path}, &bytes.Buffer{}); err == nil {
		t.Fatal("runtime validation accepted missing credentials")
	}
	engine, err := operation.Open(operation.Options{StateFile: cfg.StateFile, RecentLimit: 16})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Begin("active-operation", "ipquality.execute", "ipquality-runner"); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"check-idle", "--config", path}, &bytes.Buffer{}); err == nil {
		t.Fatal("repair ignored active execution state")
	}
}

func TestCheckIdleRejectsAnActiveOperation(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "operations.json")
	ipStatePath := filepath.Join(t.TempDir(), "ip-state.json")
	engine, err := operation.Open(operation.Options{StateFile: statePath, RecentLimit: 16})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.CheckIdle(statePath, ipStatePath, 16); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Begin("active-command", "changeip", "target-network"); err != nil {
		t.Fatal(err)
	}
	if err := app.CheckIdle(statePath, ipStatePath, 16); err == nil || !strings.Contains(err.Error(), "active") {
		t.Fatalf("checkIdle error = %v, want active operation rejection", err)
	}
	if err := app.CheckMaintenanceSafe(statePath, ipStatePath, 16); err == nil || !strings.Contains(err.Error(), "active") {
		t.Fatalf("checkMaintenanceSafe error = %v, want active operation rejection", err)
	}
}

func TestCheckConfigValidatesRuntimeDependencies(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	config := `{
  "schema_version": 3,
  "configuration_revision": 1,
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
    "ip_watch": {"enabled": true, "interval_seconds": 60, "observe_ipv6": false},
    "change_ip": {
      "provider": "command",
      "program": "/usr/local/lib/akastr-agent-providers/definitely-missing",
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

func TestRuntimeCommandsRequireManagedConfigurationPath(t *testing.T) {
	for _, command := range []string{
		"run", "enroll", "check-config", "check-idle", "capabilities", "validate-configuration",
	} {
		err := run([]string{command}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "configuration path is required") {
			t.Fatalf("%s error = %v", command, err)
		}
	}
}
