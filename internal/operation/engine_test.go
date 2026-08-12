package operation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnginePersistsMutexAndTerminalHistory(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC)
	clock := func() time.Time { return now }

	engine := openTestEngine(t, statePath, 16, clock)
	started, err := engine.Begin("op-001", "changeip", "target-network")
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if started.Status != StatusRunning || !started.StartedAt.Equal(now) {
		t.Fatalf("Begin() record = %#v", started)
	}
	if _, err := engine.Begin("op-002", "ipquality", "target-network"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second Begin() error = %v, want conflict", err)
	}

	reopened := openTestEngine(t, statePath, 16, clock)
	if len(reopened.Snapshot().Active) != 1 {
		t.Fatalf("reopened active = %#v", reopened.Snapshot().Active)
	}
	now = now.Add(time.Minute)
	finished, err := reopened.Finish("op-001", StatusSucceeded, "changed")
	if err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	if finished.FinishedAt == nil || !finished.FinishedAt.Equal(now) {
		t.Fatalf("Finish() record = %#v", finished)
	}

	final := openTestEngine(t, statePath, 16, clock).Snapshot()
	if len(final.Active) != 0 || len(final.Recent) != 1 || final.Recent[0].TerminalCode != "changed" {
		t.Fatalf("final snapshot = %#v", final)
	}
}

func TestEngineRejectsDuplicateOperationID(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	engine := openTestEngine(t, statePath, 16, time.Now)
	if _, err := engine.Begin("op-001", "changeip", "target-network"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Finish("op-001", StatusFailed, "provider_failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Begin("op-001", "changeip", "target-network"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("Begin() error = %v, want duplicate", err)
	}
}

func TestEngineBoundsRecentHistory(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	engine := openTestEngine(t, statePath, 2, time.Now)
	for _, id := range []string{"op-001", "op-002", "op-003"} {
		if _, err := engine.Begin(id, "changeip", "target-network"); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Finish(id, StatusSucceeded, "changed"); err != nil {
			t.Fatal(err)
		}
	}
	recent := engine.Snapshot().Recent
	if len(recent) != 2 || recent[0].OperationID != "op-002" || recent[1].OperationID != "op-003" {
		t.Fatalf("recent = %#v", recent)
	}
}

func TestEngineFailsClosedOnUnknownState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"schema_version":2,"active":{},"recent":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{StateFile: statePath, RecentLimit: 16}); err == nil {
		t.Fatal("Open() accepted unknown state schema")
	}
}

func openTestEngine(t *testing.T, statePath string, limit int, clock func() time.Time) *Engine {
	t.Helper()
	engine, err := Open(Options{StateFile: statePath, RecentLimit: limit, Now: clock})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return engine
}
