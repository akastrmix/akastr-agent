package autoupdate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/akastrmix/akastr-agent/internal/bootstrap"
	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type loopClient struct{ called chan struct{} }

func (client loopClient) Check(ctx context.Context, _ string, _ string, _ int64, _ identity.Identity) (Manifest, error) {
	select {
	case client.called <- struct{}{}:
	case <-ctx.Done():
		return Manifest{}, ctx.Err()
	}
	return Manifest{
		Schema: Schema, Status: "current",
		Software: SoftwareTarget{
			Status: "current", Version: "v1.0.6", Protocol: protocol.Version,
			BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v1.0.6/akastr-agent-linux-amd64",
			BinarySHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		Configuration: ConfigurationTarget{Status: "current", Revision: 1, SchemaVersion: bootstrap.SchemaVersion, MinimumAgentVersion: "v1.0.6"},
	}, nil
}
func (loopClient) FetchConfiguration(context.Context, string, int64, identity.Identity, string) (Configuration, error) {
	return Configuration{}, errors.New("unexpected fetch")
}
func (loopClient) Report(context.Context, string, string, identity.Identity, MaintenanceResult) error {
	return nil
}
func TestReconcileOnceDoesNotReexecWhenTargetsAreCurrent(t *testing.T) {
	called := make(chan struct{}, 1)
	reexec := false
	changed, err := ReconcileOnce(t.Context(), LoopOptions{
		ControlEndpoint: "wss://control.example/internal/agents/ws", CurrentVersion: "v1.0.6",
		ConfigurationRevision: 1, ConfigPath: "/var/lib/akastr-agent/configurations/1/config.json",
		ReleaseRoot: t.TempDir(), Lifecycle: lifecycle.New(), Client: loopClient{called: called},
		Reexec: func(string, string, string, int64) error { reexec = true; return nil },
	})
	if err != nil || changed || reexec {
		t.Fatalf("changed=%v reexec=%v err=%v", changed, reexec, err)
	}
}

type reconciliationClient struct {
	checkFailures int
	configuration Configuration
	manifest      *Manifest
	results       []MaintenanceResult
}

func (client *reconciliationClient) Check(context.Context, string, string, int64, identity.Identity) (Manifest, error) {
	if client.checkFailures > 0 {
		client.checkFailures--
		return Manifest{}, errors.New("temporary check failure")
	}
	if client.manifest != nil {
		return *client.manifest, nil
	}
	return Manifest{
		Schema: Schema, Status: "update_available",
		Software: SoftwareTarget{
			Status: "current", Version: "v1.0.6", Protocol: protocol.Version,
			BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v1.0.6/akastr-agent-linux-amd64",
			BinarySHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		Configuration: ConfigurationTarget{Status: "update_available", Revision: 2, SchemaVersion: bootstrap.SchemaVersion, MinimumAgentVersion: "v1.0.6"},
	}, nil
}
func (client *reconciliationClient) FetchConfiguration(context.Context, string, int64, identity.Identity, string) (Configuration, error) {
	return client.configuration, nil
}
func (client *reconciliationClient) Report(_ context.Context, _ string, _ string, _ identity.Identity, result MaintenanceResult) error {
	client.results = append(client.results, result)
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
	if err == nil && len(arguments) > 0 && arguments[0] == "validate-configuration" {
		var document map[string]any
		if err := json.Unmarshal([]byte(result), &document); err != nil {
			return "", err
		}
		document["capabilities"] = []any{map[string]any{"name": "future.capability", "version": 99, "future_field": true}}
		encoded, err := json.Marshal(document)
		return string(encoded), err
	}
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

type jointUpdateRunner struct {
	materializeRunner
	checkConfigCalls int
}

func (runner *jointUpdateRunner) Output(ctx context.Context, binary string, arguments ...string) (string, error) {
	if len(arguments) == 0 {
		return "", errors.New("candidate command is missing")
	}
	switch arguments[0] {
	case "version":
		return "v1.0.7\n", nil
	case "check-config":
		runner.checkConfigCalls++
		return "", errors.New("current configuration is unsupported")
	default:
		return runner.materializeRunner.Output(ctx, binary, arguments...)
	}
}

func TestReconcileOnceMaterializesAndReexecsOneConfigurationTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deployment activation requires Unix symlinks")
	}
	root := t.TempDir()
	agentID := "123e4567-e89b-42d3-a456-426614174000"
	observeIPv6 := true
	payload := bootstrap.Payload{
		SchemaVersion: bootstrap.SchemaVersion, ConfigurationRevision: 2, Mode: "target", AgentID: agentID,
		Name: "target", ControlEndpoint: "wss://control.example/internal/agents/ws",
		Target: &bootstrap.Target{
			IPWatchIntervalSeconds: 60,
			ObserveIPv6:            &observeIPv6,
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
		BootstrapSchemaVersion: bootstrap.SchemaVersion, MinimumAgentVersion: "v1.0.6", Bootstrap: raw,
	}}
	release := filepath.Join(root, "releases", "v1.0.6")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "akastr-agent"), []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	currentDeployment := filepath.Join(root, "deployments", "v1.0.6-r1")
	if err := os.MkdirAll(currentDeployment, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(currentDeployment, filepath.Join(root, "current")); err != nil {
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
	if err != nil || !changed || !reexecuted {
		t.Fatalf("changed=%v reexecuted=%v err=%v", changed, reexecuted, err)
	}
}

func TestReconcileOnceJointUpdateValidatesOnlyCandidateConfiguration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deployment activation requires Unix symlinks")
	}
	root := t.TempDir()
	agentID := "123e4567-e89b-42d3-a456-426614174000"
	observeIPv6 := true
	payload := bootstrap.Payload{
		SchemaVersion: bootstrap.SchemaVersion, ConfigurationRevision: 2, Mode: "target", AgentID: agentID,
		Name: "target", ControlEndpoint: "wss://control.example/internal/agents/ws",
		Target: &bootstrap.Target{
			IPWatchIntervalSeconds: 60,
			ObserveIPv6:            &observeIPv6,
			ChangeIP:               bootstrap.ChangeIP{Provider: "disabled"},
			SOCKS5:                 bootstrap.SOCKS5{Enabled: true, Port: 1080},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	binary := "joint-update-agent-binary"
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(binary)))
	manifest := Manifest{
		Schema: Schema, Status: "update_available",
		Software: SoftwareTarget{
			Status: "update_available", Version: "v1.0.7", Protocol: protocol.Version,
			BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v1.0.7/akastr-agent-linux-amd64",
			BinarySHA256: checksum,
		},
		Configuration: ConfigurationTarget{
			Status: "update_available", Revision: 2, SchemaVersion: bootstrap.SchemaVersion, MinimumAgentVersion: "v1.0.7",
		},
	}
	client := &reconciliationClient{
		manifest: &manifest,
		configuration: Configuration{
			Schema: ConfigurationSchema, ConfigurationRevision: 2,
			BootstrapSchemaVersion: bootstrap.SchemaVersion, MinimumAgentVersion: "v1.0.7", Bootstrap: raw,
		},
	}
	releases := filepath.Join(root, "releases")
	currentDeployment := filepath.Join(root, "deployments", "v1.0.6-r1")
	if err := os.MkdirAll(releases, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(currentDeployment, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(currentDeployment, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	currentConfig := filepath.Join(root, "current-config.json")
	if err := os.WriteFile(currentConfig, []byte(`{"schema_version":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &jointUpdateRunner{}
	reexecuted := false
	changed, err := ReconcileOnce(t.Context(), LoopOptions{
		ControlEndpoint: payload.ControlEndpoint, CurrentVersion: "v1.0.6", ConfigurationRevision: 1,
		Credentials: identity.Identity{AgentID: agentID}, ConfigPath: currentConfig,
		ConfigurationRoot: filepath.Join(root, "configurations"), ReleaseRoot: root,
		Lifecycle: lifecycle.New(), Client: client, Runner: runner,
		Stage: func(ctx context.Context, options ApplyOptions) (StagedRelease, error) {
			options.HTTPClient = &http.Client{Transport: responseTransport{body: binary}}
			return Stage(ctx, options)
		},
		Reexec: func(binaryPath, configPath, version string, revision int64) error {
			reexecuted = version == "v1.0.7" && revision == 2 &&
				filepath.Base(binaryPath) == "akastr-agent" && filepath.Base(configPath) == "config.json"
			return nil
		},
	})
	if err != nil || !changed || !reexecuted || runner.checkConfigCalls != 0 {
		t.Fatalf("changed=%v reexecuted=%v check_config_calls=%d err=%v", changed, reexecuted, runner.checkConfigCalls, err)
	}
}

func TestReconcileOnceDoesNotRetryUncommittedImmutableTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deployment activation requires Unix symlinks")
	}
	root := t.TempDir()
	current := filepath.Join(root, "deployments", "v1.0.6-r1")
	failed := filepath.Join(root, "deployments", "v1.0.6-r2")
	if err := os.MkdirAll(current, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(failed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(current, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "maintenance-attempt.json"), []byte(`{"schema":1,"target":"v1.0.6-r2","attempts":2,"retry_id":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &reconciliationClient{}
	changed, err := ReconcileOnce(t.Context(), LoopOptions{
		ControlEndpoint: "wss://control.example/internal/agents/ws", CurrentVersion: "v1.0.6",
		ConfigurationRevision: 1, ConfigPath: filepath.Join(current, "config", "config.json"),
		ReleaseRoot: root, Lifecycle: lifecycle.New(), Client: client,
		Reexec: func(string, string, string, int64) error {
			t.Fatal("failed immutable target was retried")
			return nil
		},
	})
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(client.results) != 1 || client.results[0].Status != "suppressed" ||
		client.results[0].ErrorCode != "trial_suppressed_after_failure" ||
		client.results[0].TargetConfigurationRevision != 2 {
		t.Fatalf("unexpected maintenance results %#v", client.results)
	}
}

func TestManualRetrySurvivesCheckFailureBusyAndRestart(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("release activation requires Linux")
	}
	root, configRoot, _ := releaseFixture(t)
	if err := os.WriteFile(filepath.Join(root, "maintenance-attempt.json"), []byte(`{"schema":1,"target":"v0.7.1-r1","attempts":2,"retry_id":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := "candidate-binary"
	manifest := manifestForApply(fmt.Sprintf("%x", sha256.Sum256([]byte(binary))))
	client := &reconciliationClient{manifest: &manifest, checkFailures: 1}
	busy := true
	executions := 0
	options := LoopOptions{ControlEndpoint: "wss://control.example/internal/agents/ws", CurrentVersion: "v0.7.0", ConfigurationRevision: 1,
		ConfigPath: filepath.Join(configRoot, "1", "config.json"), ReleaseRoot: root, Lifecycle: lifecycle.New(),
		Client: client, Runner: &fakeRunner{}, Retry: &RetryState{manualID: "one-manual-click"},
		CheckIdle: func() error {
			if busy {
				return errors.New("operation active")
			}
			return nil
		},
		Stage: func(ctx context.Context, apply ApplyOptions) (StagedRelease, error) {
			apply.HTTPClient = &http.Client{Transport: responseTransport{body: binary}}
			return Stage(ctx, apply)
		},
		Reexec: func(string, string, string, int64) error { executions++; return nil },
	}
	if _, err := ReconcileOnce(t.Context(), options); err == nil || options.Retry.manualID == "" {
		t.Fatal("check failure consumed manual request")
	}
	if changed, err := ReconcileOnce(t.Context(), options); err != nil || changed || executions != 0 {
		t.Fatalf("busy update executed: %v", err)
	}
	saved, _, err := loadAttempts(root, "v0.7.1", 1, "")
	if err != nil || saved.record.Attempts != 1 {
		t.Fatalf("busy lost persisted grant: %v", err)
	}
	busy = false
	options.Retry = &RetryState{} // process restart: only durable authorization remains
	if changed, err := ReconcileOnce(t.Context(), options); err != nil || !changed || executions != 1 {
		t.Fatalf("authorized retry did not resume: %v", err)
	}
	options.Retry = &RetryState{}
	if changed, err := ReconcileOnce(t.Context(), options); err != nil || changed || executions != 1 {
		t.Fatalf("manual grant reused after restart: %v", err)
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
	_, err := materializeCandidate(
		t.Context(), materializeRunner{}, "unused", root,
		Configuration{ConfigurationRevision: 2, Bootstrap: []byte(`{"desired":"different"}`)},
		"123e4567-e89b-42d3-a456-426614174000",
	)
	if err == nil || !strings.Contains(err.Error(), "does not match desired bootstrap") {
		t.Fatalf("err=%v", err)
	}
}
