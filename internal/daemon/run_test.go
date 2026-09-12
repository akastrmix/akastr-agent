package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/app"
	"github.com/akastrmix/akastr-agent/internal/autoupdate"
	"github.com/akastrmix/akastr-agent/internal/config"
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

func missingDependencyModel(t *testing.T) *app.Model {
	t.Helper()
	root := t.TempDir()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentials := identity.Identity{
		SchemaVersion: identity.SchemaVersion, EnrollmentState: identity.EnrollmentConfirmed,
		AgentID:   "123e4567-e89b-42d3-a456-426614174000",
		PublicKey: base64.RawURLEncoding.EncodeToString(public), PrivateKey: base64.RawURLEncoding.EncodeToString(private),
	}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	credentialFile := filepath.Join(root, "identity.json")
	if err := os.WriteFile(credentialFile, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	return &app.Model{Config: config.Config{
		ConfigurationRevision: 1,
		Node:                  config.NodeConfig{ID: credentials.AgentID},
		Control:               config.ControlConfig{CredentialFile: credentialFile},
		StateFile:             filepath.Join(root, "operations.json"), IPStateFile: filepath.Join(root, "ip.json"), RecentOperationLimit: 16,
		Capabilities: config.CapabilitiesConfig{ChangeIP: config.ChangeIPConfig{
			Provider: "command", Program: filepath.Join(root, "missing-provider"), TimeoutSeconds: 30,
		}},
	}}
}

func TestCurrentMissingDependencyKeepsHTTPSMaintenanceRunning(t *testing.T) {
	t.Setenv(autoupdate.TrialVersionEnvironment, "")
	t.Setenv("NOTIFY_SOCKET", "")
	model := missingDependencyModel(t)
	if _, err := app.BuildRuntime(model); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture must fail because its provider is missing: %v", err)
	}
	checked := make(chan struct{}, 1)
	var waits, business atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		switch r.URL.Path {
		case "/internal/agents/maintenance-wait":
			if waits.Add(1) > 1 {
				<-r.Context().Done()
				return
			}
			w.Header().Set("X-Akastr-Agent-Retry", "123e4567-e89b-42d3-a456-426614174001")
			_ = json.NewEncoder(w).Encode(map[string]bool{"check": true})
		case "/internal/agents/maintenance":
			_ = json.NewEncoder(w).Encode(autoupdate.Manifest{
				Schema: autoupdate.Schema, Status: "current",
				Software: autoupdate.SoftwareTarget{Status: "current", Version: "v1.0.0", Protocol: protocol.Version,
					BinaryURL: "https://github.com/akastrmix/akastr-agent/releases/download/v1.0.0/akastr-agent-linux-amd64", BinarySHA256: strings.Repeat("a", 64)},
				Configuration: autoupdate.ConfigurationTarget{Status: "current", Revision: 1, SchemaVersion: 1, MinimumAgentVersion: "v1.0.0"},
			})
			select {
			case checked <- struct{}{}:
			default:
			}
		default:
			business.Add(1)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	// The production client uses the default transport; trust only this local test server.
	previousTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = previousTransport }()
	model.Config.Control.Endpoint = "wss" + strings.TrimPrefix(server.URL, "https") + "/internal/agents/ws"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, model, Options{Version: "v1.0.0", ConfigPath: filepath.Join(t.TempDir(), "config.json"),
			Reexec: func(string, string, string, int64) error { return errors.New("unexpected process replacement") }}, t.TempDir())
	}()
	select {
	case <-checked:
	case err := <-done:
		t.Fatalf("current deployment stopped before receiving maintenance: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("missing runtime dependency blocked HTTPS maintenance")
	}
	select {
	case err := <-done:
		t.Fatalf("maintenance stopped after its first check: %v", err)
	default:
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if waits.Load() == 0 || business.Load() != 0 {
		t.Fatalf("waits=%d business requests=%d", waits.Load(), business.Load())
	}
}

func TestInvalidTrialDoesNotFallBackToMaintenance(t *testing.T) {
	t.Setenv(autoupdate.TrialVersionEnvironment, "v2.0.0")
	model := missingDependencyModel(t)
	err := run(t.Context(), model, Options{Version: "v1.0.0"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "trial version is invalid") {
		t.Fatalf("trial validation must precede runtime fallback: %v", err)
	}
}

func TestTrialMissingDependencyStopsBeforeMaintenance(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("managed trial executable paths require Linux")
	}
	if root := os.Getenv("AKASTR_TEST_TRIAL_ROOT"); root != "" {
		model := missingDependencyModel(t)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		err := run(ctx, model, Options{Version: "v1.0.0",
			ConfigPath: filepath.Join(root, "deployments", "v1.0.0-r1", "config", "config.json")}, root)
		if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "missing-provider") {
			t.Fatalf("trial runtime failure must return without maintenance fallback: %v", err)
		}
		return
	}
	root := t.TempDir()
	for _, directory := range []string{"releases/v1.0.0", "deployments/v1.0.0-r1", "configurations/1"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "configurations", "1"), filepath.Join(root, "deployments", "v1.0.0-r1", "config")); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(root, "releases", "v1.0.0", "akastr-agent")
	if err := os.WriteFile(candidate, binary, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), candidate, "-test.run=^TestTrialMissingDependencyStopsBeforeMaintenance$")
	cmd.Env = append(os.Environ(), "AKASTR_TEST_TRIAL_ROOT="+root,
		autoupdate.TrialVersionEnvironment+"=v1.0.0", autoupdate.TrialRevisionEnvironment+"=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("trial process: %v\n%s", err, output)
	}
}
