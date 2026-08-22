package autoupdate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/bootstrap"
	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type loopClient struct{ called chan struct{} }

func (client loopClient) Check(context.Context, string, string, int64, identity.Identity) (Manifest, error) {
	client.called <- struct{}{}
	return Manifest{
		Schema: Schema, Status: "current",
		Software: SoftwareTarget{
			Status: "current", Version: "v1.0.6", Protocol: protocol.Version,
			BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v1.0.6/akastr-agent-linux-amd64",
			BinarySHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		Configuration: ConfigurationTarget{Status: "current", Revision: 1, SchemaVersion: 3, MinimumAgentVersion: "v1.0.6"},
	}, nil
}
func (loopClient) FetchConfiguration(context.Context, string, int64, identity.Identity, string) (Configuration, error) {
	return Configuration{}, errors.New("unexpected fetch")
}
func (loopClient) AcceptConfiguration(context.Context, string, string, int64, []capability.Descriptor, identity.Identity) error {
	return errors.New("unexpected acceptance")
}

func TestRunLoopWaitsForReadyBeforePeriodicMaintenance(t *testing.T) {
	ready := make(chan struct{})
	called := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunLoop(ctx, LoopOptions{
			ControlEndpoint: "wss://control.example/internal/agents/ws", CurrentVersion: "v1.0.6",
			ConfigurationRevision: 1, ConfigPath: "/etc/akastr-agent/config.json",
			ReleaseRoot: "/usr/local/lib/akastr-agent", Lifecycle: lifecycle.New(),
			Ready: ready, Client: loopClient{called: called}, InitialDelay: func() time.Duration { return 0 },
			Reexec: func(string, string, string, int64) error { return nil },
		})
	}()
	select {
	case <-called:
		t.Fatal("maintenance checked before ready")
	case <-time.After(20 * time.Millisecond):
	}
	close(ready)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not check after ready")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("loop error=%v", err)
	}
}

func TestRunLoopManualTriggerSkipsInitialDelayAfterReady(t *testing.T) {
	ready := make(chan struct{})
	triggers := make(chan struct{}, 1)
	called := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunLoop(ctx, LoopOptions{
			ControlEndpoint: "wss://control.example/internal/agents/ws", CurrentVersion: "v1.0.6",
			ConfigurationRevision: 1, ConfigPath: "/etc/akastr-agent/config.json",
			ReleaseRoot: "/usr/local/lib/akastr-agent", Lifecycle: lifecycle.New(),
			Ready: ready, Triggers: triggers, Client: loopClient{called: called},
			InitialDelay: func() time.Duration { return time.Hour },
			Reexec:       func(string, string, string, int64) error { return nil },
		})
	}()
	triggers <- struct{}{}
	select {
	case <-called:
		t.Fatal("manual maintenance checked before control readiness")
	case <-time.After(20 * time.Millisecond):
	}
	close(ready)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("manual maintenance trigger did not skip the initial delay")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("loop error=%v", err)
	}
}

func TestReconcileOnceDoesNotReexecWhenTargetsAreCurrent(t *testing.T) {
	called := make(chan struct{}, 1)
	reexec := false
	changed, err := ReconcileOnce(t.Context(), LoopOptions{
		ControlEndpoint: "wss://control.example/internal/agents/ws", CurrentVersion: "v1.0.6",
		ConfigurationRevision: 1, ConfigPath: "/etc/akastr-agent/config.json",
		ReleaseRoot: "/usr/local/lib/akastr-agent", Lifecycle: lifecycle.New(), Client: loopClient{called: called},
		Reexec: func(string, string, string, int64) error { reexec = true; return nil },
	})
	if err != nil || changed || reexec {
		t.Fatalf("changed=%v reexec=%v err=%v", changed, reexec, err)
	}
}

type reconciliationClient struct {
	configuration Configuration
	accepted      bool
}

func (client *reconciliationClient) Check(context.Context, string, string, int64, identity.Identity) (Manifest, error) {
	return Manifest{
		Schema: Schema, Status: "update_available",
		Software: SoftwareTarget{
			Status: "current", Version: "v1.0.6", Protocol: protocol.Version,
			BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v1.0.6/akastr-agent-linux-amd64",
			BinarySHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		Configuration: ConfigurationTarget{Status: "update_available", Revision: 2, SchemaVersion: 3, MinimumAgentVersion: "v1.0.6"},
	}, nil
}
func (client *reconciliationClient) FetchConfiguration(context.Context, string, int64, identity.Identity, string) (Configuration, error) {
	return client.configuration, nil
}
func (client *reconciliationClient) AcceptConfiguration(_ context.Context, _ string, version string, revision int64, capabilities []capability.Descriptor, _ identity.Identity) error {
	if version != "v1.0.6" || revision != 2 || len(capabilities) != 2 {
		return errors.New("unexpected acceptance")
	}
	client.accepted = true
	return nil
}

type materializeRunner struct{}

func (materializeRunner) Output(_ context.Context, _ string, arguments ...string) (string, error) {
	if len(arguments) == 3 && arguments[0] == "validate-configuration" && arguments[1] == "--config" {
		configBytes, err := os.ReadFile(arguments[2])
		if err != nil {
			return "", err
		}
		var config struct {
			ConfigurationRevision int64 `json:"configuration_revision"`
			Node                  struct {
				ID string `json:"id"`
			} `json:"node"`
		}
		if err := json.Unmarshal(configBytes, &config); err != nil {
			return "", err
		}
		encoded, err := json.Marshal(struct {
			AgentID               string                  `json:"agent_id"`
			ConfigurationRevision int64                   `json:"configuration_revision"`
			Capabilities          []capability.Descriptor `json:"capabilities"`
		}{
			AgentID: config.Node.ID, ConfigurationRevision: config.ConfigurationRevision,
			Capabilities: []capability.Descriptor{
				{Name: "ip.observe", Version: 1},
				{Name: "proxy.socks5", Version: 1},
			},
		})
		return string(encoded), err
	}
	if len(arguments) != 11 || arguments[0] != "materialize-configuration" {
		return "", errors.New("unexpected candidate command")
	}
	values := map[string]string{}
	for index := 1; index < len(arguments); index += 2 {
		values[arguments[index]] = arguments[index+1]
	}
	raw, err := os.ReadFile(values["--input"])
	if err != nil {
		return "", err
	}
	revision, err := strconv.ParseInt(values["--revision"], 10, 64)
	if err != nil {
		return "", err
	}
	_, err = bootstrap.MaterializeConfiguration(values["--output-dir"], values["--runtime-dir"], raw, values["--agent-id"], revision)
	return "configuration materialized", err
}

type futureConfigurationRunner struct{ materializeRunner }

func (runner futureConfigurationRunner) Output(ctx context.Context, binary string, arguments ...string) (string, error) {
	result, err := runner.materializeRunner.Output(ctx, binary, arguments...)
	if err != nil || len(arguments) == 0 || arguments[0] != "materialize-configuration" {
		return result, err
	}
	values := map[string]string{}
	for index := 1; index < len(arguments); index += 2 {
		values[arguments[index]] = arguments[index+1]
	}
	configPath := filepath.Join(values["--output-dir"], "config.json")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "", err
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return "", err
	}
	document["future_candidate_only_field"] = true
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	return result, os.WriteFile(configPath, encoded, 0o600)
}

func TestReconcileOnceMaterializesAcceptsAndReexecsOneConfigurationTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deployment activation requires Unix symlinks")
	}
	root := t.TempDir()
	agentID := "123e4567-e89b-42d3-a456-426614174000"
	payload := bootstrap.Payload{
		SchemaVersion: 3, ConfigurationRevision: 2, Mode: "target", AgentID: agentID,
		Name: "target", ControlEndpoint: "wss://control.example/internal/agents/ws",
		Target: &bootstrap.Target{
			IPWatchIntervalSeconds: 60,
			ChangeIP:               bootstrap.ChangeIP{Provider: "disabled"},
			SOCKS5:                 bootstrap.SOCKS5{Enabled: true, Port: 1080},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	client := &reconciliationClient{configuration: Configuration{
		Schema: ConfigurationSchema, ConfigurationRevision: 2,
		BootstrapSchemaVersion: 3, MinimumAgentVersion: "v1.0.6", Bootstrap: raw,
	}}
	release := filepath.Join(root, "releases", "v1.0.6")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "akastr-agent"), []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	currentConfig := filepath.Join(root, "current-config.json")
	if err := os.WriteFile(currentConfig, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	reexecuted := false
	changed, err := ReconcileOnce(t.Context(), LoopOptions{
		ControlEndpoint: payload.ControlEndpoint, CurrentVersion: "v1.0.6", ConfigurationRevision: 1,
		Credentials: identity.Identity{AgentID: agentID}, ConfigPath: currentConfig,
		ConfigurationRoot: filepath.Join(root, "configurations"), ReleaseRoot: root,
		Lifecycle: lifecycle.New(), Client: client, Runner: futureConfigurationRunner{},
		Reexec: func(binary, config, version string, revision int64) error {
			reexecuted = version == "v1.0.6" && revision == 2 && filepath.Base(binary) == "akastr-agent" && filepath.Base(config) == "config.json"
			return nil
		},
	})
	if err != nil || !changed || !client.accepted || !reexecuted {
		t.Fatalf("changed=%v accepted=%v reexecuted=%v err=%v", changed, client.accepted, reexecuted, err)
	}
}

func TestMaterializeCandidateRejectsAReusedRevisionWithDifferentBootstrap(t *testing.T) {
	root := filepath.Join(t.TempDir(), "configurations")
	target := filepath.Join(root, "2")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(target, bootstrap.ConfigurationBootstrapDigestFile),
		[]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	_, _, err := materializeCandidate(
		t.Context(), materializeRunner{}, "unused", root,
		Configuration{ConfigurationRevision: 2, Bootstrap: []byte(`{"desired":"different"}`)},
		"123e4567-e89b-42d3-a456-426614174000",
	)
	if err == nil || !strings.Contains(err.Error(), "does not match desired bootstrap") {
		t.Fatalf("err=%v", err)
	}
}
