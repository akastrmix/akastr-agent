package autoupdate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
)

type scheduledClient struct {
	calls    atomic.Int64
	failures int
}

func (c *scheduledClient) Check(context.Context, string, string, int64, identity.Identity) (Manifest, error) {
	c.calls.Add(1)
	if c.failures > 0 {
		c.failures--
		return Manifest{}, errors.New("offline")
	}
	return Manifest{Status: "current"}, nil
}
func (*scheduledClient) FetchConfiguration(context.Context, string, int64, identity.Identity, string) (Configuration, error) {
	return Configuration{}, errors.New("unexpected fetch")
}
func (*scheduledClient) Report(context.Context, string, string, identity.Identity, MaintenanceResult) error {
	return nil
}

func TestSchedulerCoalescesStartupAndRetriesWithoutNotifications(t *testing.T) {
	root := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := &scheduledClient{failures: 1}
		triggers := make(chan Trigger, 1)
		done := make(chan error, 1)
		go func() {
			done <- RunLoop(ctx, LoopOptions{ControlEndpoint: "wss://control.invalid", CurrentVersion: "v1.0.0", ConfigurationRevision: 1, ConfigPath: "unused", ReleaseRoot: root, Lifecycle: lifecycle.New(), Client: client, Triggers: triggers, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Reexec: func(string, string, string, int64) error { return nil }})
		}()
		for range 8 {
			Notify(triggers, Trigger{})
			synctest.Wait()
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if client.calls.Load() != 1 {
			t.Fatalf("startup burst made %d checks", client.calls.Load())
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if client.calls.Load() != 2 {
			t.Fatalf("offline retry made %d checks", client.calls.Load())
		}
		triggers <- Trigger{RetryID: "manual"}
		synctest.Wait()
		if client.calls.Load() != 3 {
			t.Fatalf("manual check delayed: %d", client.calls.Load())
		}
		cancel()
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestSchedulerIdleDayHasOnlyFourPeriodicChecks(t *testing.T) {
	root := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := &scheduledClient{}
		done := make(chan error, 1)
		go func() {
			done <- RunLoop(ctx, LoopOptions{ControlEndpoint: "wss://control.invalid", CurrentVersion: "v1.0.0", ConfigurationRevision: 1, ConfigPath: "unused", ReleaseRoot: root, Lifecycle: lifecycle.New(), Client: client, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Reexec: func(string, string, string, int64) error { return nil }})
		}()
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		if client.calls.Load() != 4 {
			t.Fatalf("idle day checks: %d", client.calls.Load())
		}
		cancel()
		synctest.Wait()
		<-done
	})
}
