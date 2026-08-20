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

func TestStageLeavesCurrentUntouchedAndCommitRetainsPrevious(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("symlink release activation is Linux-only")
	}
	root, previous := releaseFixture(t)
	binary := "future-agent-binary"
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(binary)))
	runner := &fakeRunner{}
	staged, err := Stage(t.Context(), ApplyOptions{
		Manifest: manifestForApply(checksum), ConfigPath: filepath.Join(root, "config.json"),
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
	result, err := Commit(CommitOptions{Version: staged.Version, ReleaseRoot: root})
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
	if current != filepath.Join(root, "releases", "v0.7.1") {
		t.Fatalf("unexpected activation current=%s", current)
	}
	if _, err := os.Stat(previous); err != nil {
		t.Fatal("previous release was not retained")
	}
	if _, err := os.Stat(filepath.Join(root, "releases", "v0.5.0")); !os.IsNotExist(err) {
		t.Fatal("stale third release was not removed")
	}
}

func TestCommitNeverDeletesTargetAfterRenameWhenDirectorySyncFails(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("symlink release activation is Linux-only")
	}
	root, previous := releaseFixture(t)
	target := filepath.Join(root, "releases", "v0.7.1")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "akastr-agent"), []byte("future"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := errors.New("fsync failed")
	result, err := Commit(CommitOptions{
		Version: "v0.7.1", ReleaseRoot: root,
		SyncDirectory: func(string) error { return want },
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

func TestStageSyncsBinaryDirectoryBeforePublishingRelease(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("symlink release activation is Linux-only")
	}
	root, _ := releaseFixture(t)
	binary := "future-agent-binary"
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(binary)))
	var synced []string
	_, err := Stage(t.Context(), ApplyOptions{
		Manifest: manifestForApply(checksum), ConfigPath: filepath.Join(root, "config.json"),
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

func releaseFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	releases := filepath.Join(root, "releases")
	previous := filepath.Join(releases, "v0.7.0")
	if err := os.MkdirAll(previous, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(releases, "v0.5.0"), 0o755); err != nil {
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
		Schema: Schema, Status: "update_available", Version: "v0.7.1",
		Protocol:     protocol.Version,
		BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v0.7.1/akastr-agent-linux-amd64",
		BinarySHA256: checksum,
	}
}
