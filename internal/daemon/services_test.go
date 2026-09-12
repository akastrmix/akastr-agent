package daemon

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCurrentReadinessDoesNotWaitForNetworkMaintenance(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	notified, maintaining := make(chan struct{}), make(chan struct{})
	var stopped atomic.Int32
	wait := func(ctx context.Context) error { <-ctx.Done(); stopped.Add(1); return ctx.Err() }
	s := serviceGroup{
		ready: make(chan struct{}), notify: func() error { close(notified); return nil },
		control:     wait,
		maintenance: func(ctx context.Context) error { close(maintaining); return wait(ctx) },
		watch:       func(ctx context.Context) { _ = wait(ctx) },
	}
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("network blocked process readiness")
	}
	select {
	case <-maintaining:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not start without business readiness")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if stopped.Load() != 3 {
		t.Fatal("supervisor returned before all services stopped")
	}
}

func TestTrialTimesOutBeforeStartingAnotherMaintenance(t *testing.T) {
	var calls atomic.Int32
	discarded := false
	wait := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	s := serviceGroup{
		ready: make(chan struct{}), trialTimeout: 10 * time.Millisecond,
		notify:  func() error { calls.Add(1); return nil },
		control: wait, maintenance: func(ctx context.Context) error { calls.Add(1); return wait(ctx) },
		watch:        func(ctx context.Context) { <-ctx.Done() },
		discardTrial: func() error { discarded = true; return nil },
	}
	err := s.run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "did not reach") || !discarded || calls.Load() != 0 {
		t.Fatalf("trial err=%v discarded=%v premature callbacks=%d", err, discarded, calls.Load())
	}
}

func TestTrialReadinessStartsMaintenanceAndDisarmsDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready, started := make(chan struct{}), make(chan struct{})
	var discarded atomic.Bool
	s := serviceGroup{
		ready: ready, trialTimeout: 10 * time.Millisecond,
		control:      func(ctx context.Context) error { close(ready); <-ctx.Done(); return ctx.Err() },
		maintenance:  func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() },
		watch:        func(ctx context.Context) { <-ctx.Done() },
		discardTrial: func() error { discarded.Store(true); return nil },
	}
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("ready trial did not start maintenance")
	}
	select {
	case err := <-done:
		t.Fatalf("committed trial timed out: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil || discarded.Load() {
		t.Fatalf("err=%v discarded=%v", err, discarded.Load())
	}
}
