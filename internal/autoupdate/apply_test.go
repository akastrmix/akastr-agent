package autoupdate

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type responseTransport struct{ body string }

func (transport responseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(transport.body)),
		Header:     make(http.Header),
	}, nil
}

type fakeRunner struct {
	restarts           int
	failFirstStability bool
}

func (runner *fakeRunner) Run(_ context.Context, name string, arguments ...string) error {
	if name != "systemctl" || strings.Join(arguments, " ") != "restart akastr-agent.service" {
		return fmt.Errorf("unexpected run: %s %v", name, arguments)
	}
	runner.restarts++
	return nil
}

func (runner *fakeRunner) Output(_ context.Context, name string, arguments ...string) (string, error) {
	if strings.HasSuffix(name, "akastr-agent") {
		switch arguments[0] {
		case "version":
			return "v0.6.1\n", nil
		case "check-config":
			return "configuration valid\n", nil
		}
	}
	if name == "systemctl" && arguments[0] == "is-active" {
		if runner.failFirstStability && runner.restarts == 1 {
			return "inactive\n", nil
		}
		return "active\n", nil
	}
	if name == "systemctl" && arguments[0] == "show" {
		return "42\n", nil
	}
	return "", fmt.Errorf("unexpected output: %s %v", name, arguments)
}

func TestApplyAtomicallySwitchesAndRetainsOnlyCurrentAndPrevious(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("symlink release activation is Linux-only")
	}
	root, previous := releaseFixture(t)
	binary := "future-agent-binary"
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(binary)))
	runner := &fakeRunner{}
	err := Apply(t.Context(), ApplyOptions{
		Manifest: manifestForApply(checksum), ConfigPath: filepath.Join(root, "config.json"),
		ReleaseRoot: root,
		HTTPClient:  &http.Client{Transport: responseTransport{body: binary}},
		Runner:      runner, Sleep: func(time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := filepath.EvalSymlinks(filepath.Join(root, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if current != filepath.Join(root, "releases", "v0.6.1") || runner.restarts != 1 {
		t.Fatalf("unexpected activation current=%s restarts=%d", current, runner.restarts)
	}
	if _, err := os.Stat(previous); err != nil {
		t.Fatal("previous release was not retained")
	}
	if _, err := os.Stat(filepath.Join(root, "releases", "v0.4.0")); !os.IsNotExist(err) {
		t.Fatal("stale third release was not removed")
	}
}

func TestApplyRestoresPreviousReleaseWhenServiceIsUnhealthy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("symlink release activation is Linux-only")
	}
	root, previous := releaseFixture(t)
	binary := "future-agent-binary"
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(binary)))
	runner := &fakeRunner{failFirstStability: true}
	err := Apply(t.Context(), ApplyOptions{
		Manifest: manifestForApply(checksum), ConfigPath: filepath.Join(root, "config.json"),
		ReleaseRoot: root,
		HTTPClient:  &http.Client{Transport: responseTransport{body: binary}},
		Runner:      runner, Sleep: func(time.Duration) {},
	})
	if err == nil || !strings.Contains(err.Error(), "previous release was restored") {
		t.Fatalf("expected successful rollback error, got %v", err)
	}
	current, resolveError := filepath.EvalSymlinks(filepath.Join(root, "current"))
	if resolveError != nil || current != previous || runner.restarts != 2 {
		t.Fatalf("rollback mismatch current=%s restarts=%d error=%v", current, runner.restarts, resolveError)
	}
	if _, err := os.Stat(filepath.Join(root, "releases", "v0.6.1")); !os.IsNotExist(err) {
		t.Fatal("failed target release was not removed")
	}
}

func TestApplyDefersWhenOperationBecomesActiveBeforeSwitch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("symlink release activation is Linux-only")
	}
	root, previous := releaseFixture(t)
	binary := "future-agent-binary"
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(binary)))
	runner := &fakeRunner{}
	err := Apply(t.Context(), ApplyOptions{
		Manifest: manifestForApply(checksum), ConfigPath: filepath.Join(root, "config.json"),
		ReleaseRoot: root,
		HTTPClient:  &http.Client{Transport: responseTransport{body: binary}},
		Runner:      runner, Sleep: func(time.Duration) {},
		OperationActive: func() (bool, error) { return true, nil },
	})
	if !errors.Is(err, ErrOperationActive) {
		t.Fatalf("expected active-operation deferral, got %v", err)
	}
	current, resolveError := filepath.EvalSymlinks(filepath.Join(root, "current"))
	if resolveError != nil || current != previous || runner.restarts != 0 {
		t.Fatalf("active deferral changed release current=%s restarts=%d error=%v", current, runner.restarts, resolveError)
	}
	if _, err := os.Stat(filepath.Join(root, "releases", "v0.6.1")); !os.IsNotExist(err) {
		t.Fatal("deferred target release was not removed")
	}
}

func releaseFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	releases := filepath.Join(root, "releases")
	previous := filepath.Join(releases, "v0.6.0")
	if err := os.MkdirAll(previous, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(releases, "v0.4.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(previous, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, previous
}

func manifestForApply(checksum string) Manifest {
	return Manifest{
		Schema: Schema, Status: "update_available", Version: "v0.6.1",
		Protocol:     protocol.Version,
		BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v0.6.1/akastr-agent-linux-amd64",
		BinarySHA256: checksum,
	}
}
