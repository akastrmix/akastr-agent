package autoupdate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/lifecycle"
)

func TestCandidateFailureClassification(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestCandidateRejectionHelper")
	cmd.Env = append(os.Environ(), "AKASTR_TEST_CANDIDATE_REJECTION=1")
	err := cmd.Run()
	if !errors.Is(candidateCommandError(t.Context(), err), ErrCandidateRejected) {
		t.Fatal("exit rejection is retryable")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(candidateCommandError(ctx, err), context.Canceled) {
		t.Fatal("cancellation was classified as rejection")
	}
	if errors.Is(candidateCommandError(t.Context(), os.ErrPermission), ErrCandidateRejected) {
		t.Fatal("I/O error became permanent")
	}
}

func TestCandidateRejectionHelper(t *testing.T) {
	if os.Getenv("AKASTR_TEST_CANDIDATE_REJECTION") == "1" {
		os.Exit(1)
	}
}

func TestRetryBackoffAndTargetChange(t *testing.T) {
	now := time.Now()
	s := &RetryState{now: func() time.Time { return now }}
	if s.blocked("v1-r2") != "" {
		t.Fatal("new target is blocked")
	}
	for _, expected := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		s.failed(errors.New("temporary transport failure"))
		if s.next.Sub(now) != expected || s.blocked("v1-r2") != "maintenance_retry_wait" {
			t.Fatal("wrong backoff")
		}
		now = now.Add(expected)
		if s.blocked("v1-r2") != "" {
			t.Fatal("retry remains blocked after deadline")
		}
	}
	s.failed(ErrCandidateRejected)
	if s.blocked("v1-r2") != "candidate_target_rejected" {
		t.Fatal("rejection was not suppressed")
	}
	if s.blocked("v1-r3") != "" {
		t.Fatal("new revision did not reset suppression")
	}
}

func TestCoordinatorSuppressesRepeatedRejectedTargetBeforeDeployment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix symlinks")
	}
	root := t.TempDir()
	current := filepath.Join(root, "deployments", "v1.0.6-r1")
	if err := os.MkdirAll(current, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(current, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	manifest := manifestForApply("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	client := &reconciliationClient{manifest: &manifest}
	calls := 0
	options := LoopOptions{ControlEndpoint: "wss://control.example/internal/agents/ws", CurrentVersion: "v0.7.0",
		ConfigurationRevision: 1, ConfigPath: filepath.Join(current, "config.json"), ReleaseRoot: root,
		Lifecycle: lifecycle.New(), Client: client, Retry: &RetryState{},
		Stage: func(context.Context, ApplyOptions) (StagedRelease, error) {
			calls++
			return StagedRelease{}, ErrCandidateRejected
		},
		Reexec: func(string, string, string, int64) error { t.Fatal("rejected target executed"); return nil }}
	if _, err := ReconcileOnce(t.Context(), options); !errors.Is(err, ErrCandidateRejected) {
		t.Fatalf("err=%v", err)
	}
	if changed, err := ReconcileOnce(t.Context(), options); changed || err != nil {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if calls != 1 || client.results[len(client.results)-1].Status != "suppressed" {
		t.Fatal("target was retried")
	}
	manifest.Configuration.Revision++
	manifest.Configuration.Status = "update_available"
	_, _ = ReconcileOnce(t.Context(), options)
	if calls != 2 {
		t.Fatal("new target did not retry")
	}
}
