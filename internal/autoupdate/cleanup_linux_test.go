package autoupdate

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/akastrmix/akastr-agent/internal/lifecycle"
)

func TestMaintenanceCleanupWaitsForWriterAndRecoversAfterKill(t *testing.T) {
	root, configRoot, current := releaseFixture(t)
	if err := os.Symlink(current, filepath.Join(current, "previous")); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(root, "releases", ".update-123")
	cmd := exec.Command("/bin/sh", "-c", `exec 9>"$1/.maintenance.lock"; flock 9; mkdir "$1/releases/.update-123"; echo ready; exec sleep 60`, "writer", root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("writer: %q %v", line, err)
	}
	if lock, err := lockMaintenance(root); err == nil {
		lock.Close()
		t.Fatal("concurrent writer acquired lock")
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatal("active staging was removed")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	lock, err := lockMaintenance(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := cleanupStaging(root, configRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan remains: %v", err)
	}
}

func TestCleanupRetainsExactPredecessorAndOnlyDesiredCandidate(t *testing.T) {
	root, configRoot, current := releaseFixture(t)
	previous := filepath.Join(root, "deployments", "v0.5.0-r1")
	if err := os.WriteFile(filepath.Join(root, "releases", "v0.5.0", "akastr-agent"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, destination := range map[string]string{"akastr-agent": filepath.Join(root, "releases", "v0.5.0", "akastr-agent"), "config": filepath.Join(configRoot, "1")} {
		if err := os.Symlink(destination, filepath.Join(previous, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(previous, filepath.Join(current, "previous")); err != nil {
		t.Fatal(err)
	}
	for revision := 2; revision <= 12; revision++ {
		version := fmt.Sprintf("v0.8.%d", revision)
		if err := os.MkdirAll(filepath.Join(root, "releases", version), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(configRoot, fmt.Sprint(revision)), 0700); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{filepath.Join(root, "releases", version, "akastr-agent"), filepath.Join(configRoot, fmt.Sprint(revision), "config.json")} {
			if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := StageDeployment(root, version, int64(revision), filepath.Join(configRoot, fmt.Sprint(revision), "config.json")); err != nil {
			t.Fatal(err)
		}
		if err := cleanupCandidates(root, configRoot, version, int64(revision)); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Join(root, "deployments"))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 3 {
			t.Fatalf("candidate accumulation at revision %d: %d", revision, len(entries))
		}
	}
	for _, path := range []string{current, previous, filepath.Join(root, "releases", "manual")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected path %s: %v", path, err)
		}
	}
	if err := cleanupCandidates(root, configRoot, "", 0); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "deployments"))
	if len(entries) != 2 {
		t.Fatalf("current cleanup retained %d deployments", len(entries))
	}
}

func TestCleanupPreservesUnknownHistoryAndDoesNotFollowStagingSymlinks(t *testing.T) {
	root, configRoot, _ := releaseFixture(t)
	if err := cleanupCandidates(root, configRoot, "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "deployments", "v0.5.0-r1")); err != nil {
		t.Fatal("guessed predecessor")
	}
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "releases", ".update-123")); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaging(root, configRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("followed symlink outside managed root")
	}
}

func TestPreviousIsPublishedWithCurrentAndSurvivesRestart(t *testing.T) {
	root, configRoot, previous := releaseFixture(t)
	version := "v0.7.1"
	if err := os.MkdirAll(filepath.Join(root, "releases", version), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "releases", version, "akastr-agent"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	target, err := StageDeployment(root, version, 1, filepath.Join(configRoot, "1", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := Commit(CommitOptions{Version: version, ConfigurationRevision: 1, ReleaseRoot: root, ConfigurationRoot: configRoot})
	if err != nil || !result.Committed {
		t.Fatalf("commit %v %v", result, err)
	}
	retained, err := filepath.EvalSymlinks(filepath.Join(target, "previous"))
	if err != nil || retained != previous {
		t.Fatalf("previous %s %v", retained, err)
	}
	if err := cleanupCandidates(root, configRoot, "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(previous); err != nil {
		t.Fatal("restart lost previous deployment")
	}
}

// Benchmark actual disk cleanup, not an in-memory imitation.
func BenchmarkIdleMaintenanceCleanup(b *testing.B) {
	root := b.TempDir()
	configs := filepath.Join(root, "configurations")
	for _, path := range []string{configs, filepath.Join(root, "releases"), filepath.Join(root, "deployments")} {
		if err := os.Mkdir(path, 0700); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for b.Loop() {
		lock, err := lockMaintenance(root)
		if err != nil {
			b.Fatal(err)
		}
		err = cleanupStaging(root, configs)
		lock.Close()
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestMaintenanceDoesNotRepeatAcknowledgedFailureReports(t *testing.T) {
	root, configRoot, _ := releaseFixture(t)
	if err := os.WriteFile(filepath.Join(root, "maintenance-attempt.json"), []byte(`{"schema":1,"target":"v0.7.1-r1","attempts":2,"retry_id":""}`), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := manifestForApply("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	client := &reconciliationClient{manifest: &manifest}
	options := LoopOptions{ControlEndpoint: "wss://control.invalid", CurrentVersion: "v0.7.0", ConfigurationRevision: 1, ConfigPath: filepath.Join(configRoot, "1", "config.json"), ConfigurationRoot: configRoot, ReleaseRoot: root, Lifecycle: lifecycle.New(), Client: client, Retry: &RetryState{}, Reexec: func(string, string, string, int64) error { t.Fatal("suppressed target executed"); return nil }}
	for range 8 {
		if _, err := ReconcileOnce(t.Context(), options); err != nil {
			t.Fatal(err)
		}
	}
	if len(client.results) != 1 {
		t.Fatalf("same failure reported %d times", len(client.results))
	}
	options.Retry = &RetryState{}
	if _, err := ReconcileOnce(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	if len(client.results) != 2 {
		t.Fatal("restart must refresh the Cloud projection")
	}
}
