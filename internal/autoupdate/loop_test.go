package autoupdate

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type loopChecker struct{ manifest Manifest }

func (checker loopChecker) Check(
	context.Context, string, string, identity.Identity,
) (Manifest, error) {
	return checker.manifest, nil
}

type signalingChecker struct{ called chan struct{} }

func (checker signalingChecker) Check(
	context.Context, string, string, identity.Identity,
) (Manifest, error) {
	checker.called <- struct{}{}
	return Manifest{Status: "up_to_date"}, nil
}

func TestRunLoopWaitsForControlReadinessBeforeInitialCheck(t *testing.T) {
	ready := make(chan struct{})
	called := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunLoop(ctx, LoopOptions{
			ControlEndpoint: "wss://control.example/internal/agents/ws",
			CurrentVersion:  "v0.7.0", ConfigPath: "/etc/akastr-agent/config.json",
			ReleaseRoot: "/usr/local/lib/akastr-agent", Lifecycle: lifecycle.New(),
			Ready: ready, Checker: signalingChecker{called: called},
			InitialDelay: func() time.Duration { return 0 },
			Reexec:       func(string, string) error { return nil },
		})
	}()
	select {
	case <-called:
		t.Fatal("automatic update checked before control readiness")
	case <-time.After(20 * time.Millisecond):
	}
	close(ready)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("automatic update did not check after readiness")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("loop exit = %v", err)
	}
}

func TestRunLoopAppliesOnlyOnATickAndReexecsTheApprovedRelease(t *testing.T) {
	ticks := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(t.Context())
	applied := make(chan struct{}, 1)
	type reexecCall struct {
		binary  string
		version string
	}
	reexecuted := make(chan reexecCall, 1)
	done := make(chan error, 1)
	go func() {
		done <- RunLoop(ctx, LoopOptions{
			ControlEndpoint: "wss://control.example/internal/agents/ws",
			CurrentVersion:  "v0.7.0", Credentials: identity.Identity{},
			ConfigPath:  "/etc/akastr-agent/config.json",
			ReleaseRoot: "/usr/local/lib/akastr-agent",
			Lifecycle:   lifecycle.New(),
			Checker: loopChecker{manifest: Manifest{
				Schema: Schema, Status: "update_available", Version: "v0.7.1",
				Protocol:     protocol.Version,
				BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v0.7.1/akastr-agent-linux-amd64",
				BinarySHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}}, Ticks: ticks,
			Stage: func(context.Context, ApplyOptions) (StagedRelease, error) {
				applied <- struct{}{}
				return StagedRelease{
					Version: "v0.7.1",
					Binary:  filepath.Join("/usr/local/lib/akastr-agent", "releases", "v0.7.1", "akastr-agent"),
				}, nil
			},
			Reexec: func(binary, targetVersion string) error {
				reexecuted <- reexecCall{binary: binary, version: targetVersion}
				return nil
			},
		})
	}()

	select {
	case <-applied:
		t.Fatal("update ran before the six-hour tick")
	default:
	}
	ticks <- time.Now()
	select {
	case <-applied:
	case <-time.After(time.Second):
		t.Fatal("update did not run after a tick")
	}
	select {
	case call := <-reexecuted:
		expected := filepath.Join(
			"/usr/local/lib/akastr-agent", "releases", "v0.7.1", "akastr-agent",
		)
		if call.binary != expected {
			t.Fatalf("unexpected reexec binary %q", call.binary)
		}
		if call.version != "v0.7.1" {
			t.Fatalf("unexpected target version %q", call.version)
		}
	case <-time.After(time.Second):
		t.Fatal("approved release was not reexecuted")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("loop exit = %v", err)
	}
}

func TestRunLoopDefersWhenAnOperationIsActive(t *testing.T) {
	gate := lifecycle.New()
	operationLease, ok := gate.TryOperation()
	if !ok {
		t.Fatal("operation lease was rejected")
	}
	defer operationLease.Release()
	ticks := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	applied := make(chan struct{}, 1)
	go func() {
		_ = RunLoop(ctx, LoopOptions{
			ControlEndpoint: "wss://control.example/internal/agents/ws",
			CurrentVersion:  "v0.7.0", Credentials: identity.Identity{},
			ConfigPath:  "/etc/akastr-agent/config.json",
			ReleaseRoot: "/usr/local/lib/akastr-agent",
			Lifecycle:   gate,
			Checker: loopChecker{manifest: Manifest{
				Schema: Schema, Status: "update_available", Version: "v0.7.1",
				Protocol:     protocol.Version,
				BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v0.7.1/akastr-agent-linux-amd64",
				BinarySHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}}, Ticks: ticks,
			Stage: func(context.Context, ApplyOptions) (StagedRelease, error) {
				applied <- struct{}{}
				return StagedRelease{}, nil
			},
			Reexec: func(string, string) error { return nil },
		})
	}()
	ticks <- time.Now()
	select {
	case <-applied:
		t.Fatal("active operation did not defer the update")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRunLoopStopsWhenReexecFails(t *testing.T) {
	ticks := make(chan time.Time, 1)
	want := errors.New("reexec failed")
	done := make(chan error, 1)
	go func() {
		done <- RunLoop(t.Context(), LoopOptions{
			ControlEndpoint: "wss://control.example/internal/agents/ws",
			CurrentVersion:  "v0.7.0", Credentials: identity.Identity{},
			ConfigPath: "/etc/akastr-agent/config.json", ReleaseRoot: "/usr/local/lib/akastr-agent",
			Lifecycle: lifecycle.New(),
			Checker: loopChecker{manifest: Manifest{
				Schema: Schema, Status: "update_available", Version: "v0.7.1",
				Protocol:     protocol.Version,
				BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v0.7.1/akastr-agent-linux-amd64",
				BinarySHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}}, Ticks: ticks,
			Stage: func(context.Context, ApplyOptions) (StagedRelease, error) {
				return StagedRelease{
					Version: "v0.7.1",
					Binary:  filepath.Join("/usr/local/lib/akastr-agent", "releases", "v0.7.1", "akastr-agent"),
				}, nil
			},
			Reexec: func(string, string) error { return want },
		})
	}()
	ticks <- time.Now()
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Fatalf("RunLoop() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunLoop did not stop after reexec failure")
	}
}
