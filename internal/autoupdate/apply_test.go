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

	"github.com/akastrmix/akastr-agent/internal/bootstrap"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type responseTransport struct{ body string }

type countingTransport struct {
	body  string
	calls int
}

func (transport *countingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.calls++
	return (responseTransport{body: transport.body}).RoundTrip(request)
}

func TestStageReusesVerifiedBinaryAcrossConfigurationFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("release activation is Linux-only")
	}
	root, configRoot, _ := releaseFixture(t)
	transport := &countingTransport{body: "future-agent-binary"}
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(transport.body)))
	options := ApplyOptions{Manifest: manifestForApply(checksum),
		ConfigPath: filepath.Join(configRoot, "1", "config.json"), ReleaseRoot: root,
		HTTPClient: &http.Client{Transport: transport}, Runner: &currentConfigRejectingRunner{}}
	for range 2 {
		if _, err := Stage(t.Context(), options); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	if transport.calls != 1 {
		t.Fatalf("downloaded %d times", transport.calls)
	}
	options.Runner = &fakeRunner{}
	if _, err := Stage(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	if transport.calls != 1 {
		t.Fatal("repaired configuration downloaded the same release again")
	}
}

func (transport responseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(transport.body)),
		Header:     make(http.Header),
	}, nil
}

type fakeRunner struct{}

func (runner *fakeRunner) Output(_ context.Context, name string, arguments ...string) (string, error) {
	if strings.HasSuffix(name, "akastr-agent") {
		switch arguments[0] {
		case "version":
			return "v0.7.1\n", nil
		case "check-config":
			return "configuration valid\n", nil
		}
	}
	return "", fmt.Errorf("unexpected output: %s %v", name, arguments)
}

type currentConfigRejectingRunner struct {
	checkConfigCalls int
}

func (runner *currentConfigRejectingRunner) Output(_ context.Context, name string, arguments ...string) (string, error) {
	if !strings.HasSuffix(name, "akastr-agent") || len(arguments) == 0 {
		return "", fmt.Errorf("unexpected output: %s %v", name, arguments)
	}
	switch arguments[0] {
	case "version":
		return "v0.7.1\n", nil
	case "check-config":
		runner.checkConfigCalls++
		return "", errors.New("current configuration is unsupported")
	default:
		return "", fmt.Errorf("unexpected output: %s %v", name, arguments)
	}
}

func TestStageLeavesCurrentUntouchedAndCommitRetainsPrevious(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("symlink release activation is Linux-only")
	}
	root, configRoot, previous := releaseFixture(t)
	binary := "future-agent-binary"
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(binary)))
	runner := &fakeRunner{}
	staged, err := Stage(t.Context(), ApplyOptions{
		Manifest: manifestForApply(checksum), ConfigPath: filepath.Join(configRoot, "1", "config.json"),
		ReleaseRoot: root,
		HTTPClient:  &http.Client{Transport: responseTransport{body: binary}},
		Runner:      runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if staged.Version != "v0.7.1" || staged.Binary != filepath.Join(root, "releases", "v0.7.1", "akastr-agent") {
		t.Fatalf("unexpected staged release %#v", staged)
	}
	current, err := filepath.EvalSymlinks(filepath.Join(root, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if current != previous {
		t.Fatalf("stage changed current to %s", current)
	}
	deployment, err := StageDeployment(root, staged.Version, 1, filepath.Join(configRoot, "1", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := Commit(CommitOptions{
		Version: staged.Version, ConfigurationRevision: 1,
		ReleaseRoot: root, ConfigurationRoot: configRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Committed || result.CleanupFailed {
		t.Fatalf("unexpected commit result %#v", result)
	}
	current, err = filepath.EvalSymlinks(filepath.Join(root, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if current != deployment {
		t.Fatalf("unexpected activation current=%s", current)
	}
	if _, err := os.Stat(previous); err != nil {
		t.Fatal("previous release was not retained")
	}
	if _, err := os.Stat(filepath.Join(root, "deployments", "v0.5.0-r1")); !os.IsNotExist(err) {
		t.Fatal("stale third deployment was not removed")
	}
	if _, err := os.Stat(filepath.Join(root, "releases", "v0.5.0")); !os.IsNotExist(err) {
		t.Fatal("unreferenced release was not removed")
	}
	if _, err := os.Stat(filepath.Join(configRoot, "9")); !os.IsNotExist(err) {
		t.Fatal("unreferenced configuration was not removed")
	}
	if _, err := os.Stat(filepath.Join(root, "releases", "manual")); err != nil {
		t.Fatal("unknown release directory was removed")
	}
}

func TestStageRequiresCurrentConfigurationForSoftwareOnlyUpdate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("automatic update staging is Linux-only")
	}
	root, configRoot, _ := releaseFixture(t)
	binary := "future-agent-binary"
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(binary)))
	runner := &currentConfigRejectingRunner{}
	_, err := Stage(t.Context(), ApplyOptions{
		Manifest: manifestForApply(checksum), ConfigPath: filepath.Join(configRoot, "1", "config.json"),
		ReleaseRoot: root,
		HTTPClient:  &http.Client{Transport: responseTransport{body: binary}},
		Runner:      runner,
	})
	if err == nil || runner.checkConfigCalls != 1 {
		t.Fatalf("software-only stage error=%v check_config_calls=%d", err, runner.checkConfigCalls)
	}
}

func TestStageDefersConfigurationValidationForJointUpdate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("automatic update staging is Linux-only")
	}
	root, configRoot, _ := releaseFixture(t)
	binary := "future-agent-binary"
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(binary)))
	manifest := manifestForApply(checksum)
	manifest.Configuration.Status = "update_available"
	manifest.Configuration.Revision = 2
	runner := &currentConfigRejectingRunner{}
	staged, err := Stage(t.Context(), ApplyOptions{
		Manifest: manifest, ConfigPath: filepath.Join(configRoot, "1", "config.json"),
		ReleaseRoot: root,
		HTTPClient:  &http.Client{Transport: responseTransport{body: binary}},
		Runner:      runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner.checkConfigCalls != 0 || staged.Version != manifest.Software.Version {
		t.Fatalf("joint stage=%#v check_config_calls=%d", staged, runner.checkConfigCalls)
	}
}

func TestDiscardRemovesOnlyAnUncommittedTrialDeployment(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("symlink release activation is Linux-only")
	}
	root := t.TempDir()
	deployments := filepath.Join(root, "deployments")
	current := filepath.Join(deployments, "v1.0.0-r1")
	target := filepath.Join(deployments, "v1.0.1-r2")
	if err := os.MkdirAll(current, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(current, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	trial := &Trial{version: "v1.0.1", revision: 2, releaseRoot: root}
	if err := trial.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discarded trial still exists: %v", err)
	}
}

func TestDiscardPreservesACommittedTrialDeployment(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("symlink release activation is Linux-only")
	}
	root := t.TempDir()
	deployments := filepath.Join(root, "deployments")
	target := filepath.Join(deployments, "v1.0.1-r2")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	trial := &Trial{version: "v1.0.1", revision: 2, releaseRoot: root}
	if err := trial.Discard(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(target); err != nil || !info.IsDir() {
		t.Fatalf("committed trial was removed: info=%v err=%v", info, err)
	}
}

func TestCommitNeverDeletesTargetAfterRenameWhenDirectorySyncFails(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("symlink release activation is Linux-only")
	}
	root, configRoot, previous := releaseFixture(t)
	targetRelease := filepath.Join(root, "releases", "v0.7.1")
	if err := os.Mkdir(targetRelease, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetRelease, "akastr-agent"), []byte("future"), 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := StageDeployment(root, "v0.7.1", 1, filepath.Join(configRoot, "1", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("fsync failed")
	result, err := Commit(CommitOptions{
		Version: "v0.7.1", ConfigurationRevision: 1,
		ReleaseRoot: root, ConfigurationRoot: configRoot,
		SyncDirectory: func(path string) error {
			if path == root {
				return want
			}
			return nil
		},
	})
	if !errors.Is(err, want) || !result.Committed {
		t.Fatalf("Commit() result=%#v error=%v", result, err)
	}
	current, resolveError := filepath.EvalSymlinks(filepath.Join(root, "current"))
	if resolveError != nil || current != target {
		t.Fatalf("current=%s error=%v", current, resolveError)
	}
	if _, statError := os.Stat(target); statError != nil {
		t.Fatalf("committed target was deleted: %v", statError)
	}
	if _, statError := os.Stat(previous); statError != nil {
		t.Fatalf("previous release was deleted: %v", statError)
	}
}

func TestCommitCleanupFailureDoesNotUndoActivation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("symlink release activation is Linux-only")
	}
	root, configRoot, previous := releaseFixture(t)
	targetRelease := filepath.Join(root, "releases", "v0.7.1")
	if err := os.Mkdir(targetRelease, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetRelease, "akastr-agent"), []byte("future"), 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := StageDeployment(root, "v0.7.1", 1, filepath.Join(configRoot, "1", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	staleDeployment := filepath.Join(root, "deployments", "v0.5.0-r1")
	result, err := Commit(CommitOptions{
		Version: "v0.7.1", ConfigurationRevision: 1,
		ReleaseRoot: root, ConfigurationRoot: configRoot,
		RemoveAll: func(path string) error {
			if path == staleDeployment {
				return errors.New("injected cleanup failure")
			}
			return os.RemoveAll(path)
		},
	})
	if err != nil || !result.Committed || !result.CleanupFailed {
		t.Fatalf("Commit() result=%#v error=%v", result, err)
	}
	current, resolveError := filepath.EvalSymlinks(filepath.Join(root, "current"))
	if resolveError != nil || current != target {
		t.Fatalf("current=%s error=%v", current, resolveError)
	}
	if _, statError := os.Stat(previous); statError != nil {
		t.Fatalf("previous deployment was deleted: %v", statError)
	}
	if _, statError := os.Stat(filepath.Join(root, "releases", "v0.5.0")); statError != nil {
		t.Fatalf("artifact cleanup continued after deployment cleanup failed: %v", statError)
	}
}

func TestStageSyncsBinaryDirectoryBeforePublishingRelease(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("symlink release activation is Linux-only")
	}
	root, configRoot, _ := releaseFixture(t)
	binary := "future-agent-binary"
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(binary)))
	var synced []string
	_, err := Stage(t.Context(), ApplyOptions{
		Manifest: manifestForApply(checksum), ConfigPath: filepath.Join(configRoot, "1", "config.json"),
		ReleaseRoot: root,
		HTTPClient:  &http.Client{Transport: responseTransport{body: binary}},
		Runner:      &fakeRunner{},
		SyncDirectory: func(path string) error {
			synced = append(synced, path)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(synced) != 2 || filepath.Dir(synced[0]) != filepath.Join(root, "releases") ||
		synced[1] != filepath.Join(root, "releases") {
		t.Fatalf("unexpected directory sync order %#v", synced)
	}
}

func releaseFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	releases := filepath.Join(root, "releases")
	configRoot := filepath.Join(root, "configurations")
	previousRelease := filepath.Join(releases, "v0.7.0")
	if err := os.MkdirAll(filepath.Join(configRoot, "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configRoot, "9"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(previousRelease, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(previousRelease, "akastr-agent"), []byte("current"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "1", "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous, err := StageDeployment(root, "v0.7.0", 1, filepath.Join(configRoot, "1", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "deployments", "v0.5.0-r1")
	if err := os.Mkdir(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(releases, "v0.5.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(releases, "manual"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(previous, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	return root, configRoot, previous
}

func manifestForApply(checksum string) Manifest {
	return Manifest{
		Schema: Schema, Status: "update_available",
		Software: SoftwareTarget{
			Status: "update_available", Version: "v0.7.1", Protocol: protocol.Version,
			BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v0.7.1/akastr-agent-linux-amd64",
			BinarySHA256: checksum,
		},
		Configuration: ConfigurationTarget{
			Status: "current", Revision: 1, SchemaVersion: bootstrap.SchemaVersion, MinimumAgentVersion: "v0.7.1",
		},
	}
}
