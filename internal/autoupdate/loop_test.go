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

func TestRunLoopAppliesOnlyOnATickAndReexecsTheApprovedRelease(t *testing.T) {
	ticks := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(t.Context())
	applied := make(chan struct{}, 1)
	reexecuted := make(chan string, 1)
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
			Apply: func(context.Context, ApplyOptions) error {
				applied <- struct{}{}
				return nil
			},
			Reexec: func(binary string) error {
				reexecuted <- binary
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
	case binary := <-reexecuted:
		expected := filepath.Join(
			"/usr/local/lib/akastr-agent", "releases", "v0.7.1", "akastr-agent",
		)
		if binary != expected {
			t.Fatalf("unexpected reexec binary %q", binary)
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
			Apply: func(context.Context, ApplyOptions) error {
				applied <- struct{}{}
				return nil
			},
			Reexec: func(string) error { return nil },
		})
	}()
	ticks <- time.Now()
	select {
	case <-applied:
		t.Fatal("active operation did not defer the update")
	case <-time.After(100 * time.Millisecond):
	}
}
