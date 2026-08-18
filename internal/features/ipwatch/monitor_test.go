package ipwatch

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type sequenceObserver struct {
	values []string
	index  int
}

func (o *sequenceObserver) Observe(_ context.Context, _ Family) (Observation, error) {
	value := o.values[o.index]
	if o.index < len(o.values)-1 {
		o.index++
	}
	return Observation{
		Address:    netip.MustParseAddr(value),
		ObservedAt: time.Date(2026, 8, 13, 0, 0, o.index, 0, time.UTC),
	}, nil
}

func TestMonitorPersistsAndRetriesNaturalIPv4ChangeUntilAck(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "ip-state.json")
	observer := &sequenceObserver{values: []string{"8.8.8.8", "8.8.4.4", "1.1.1.1"}}
	monitor, err := OpenMonitor(filePath, observer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	published := []protocol.IPObservationBody{}
	snapshots := []protocol.IPSnapshotBody{}
	publishSnapshot := func(event protocol.IPSnapshotBody) error {
		snapshots = append(snapshots, event)
		return nil
	}
	publish := func(event protocol.IPObservationBody) error {
		published = append(published, event)
		return nil
	}
	publishUnchanged := func(protocol.ChangeIPUnchangedBody) error { return nil }
	if err := monitor.step(context.Background(), publishSnapshot, publish, publishUnchanged); err != nil {
		t.Fatal(err)
	}
	if len(published) != 0 || len(snapshots) != 1 || snapshots[0].Address != "8.8.8.8" {
		t.Fatalf("initial snapshot = %#v, observations = %#v", snapshots, published)
	}
	if err := monitor.step(context.Background(), publishSnapshot, publish, publishUnchanged); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || snapshots[1].SnapshotID != snapshots[0].SnapshotID {
		t.Fatalf("pending snapshot was not retried: %#v", snapshots)
	}
	reopened, err := OpenMonitor(filePath, observer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.AckSnapshot(snapshots[0].SnapshotID); err != nil {
		t.Fatal(err)
	}
	if err := reopened.step(context.Background(), publishSnapshot, publish, publishUnchanged); err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 || published[0].PreviousAddress != "8.8.8.8" || published[0].Address != "8.8.4.4" {
		t.Fatalf("published = %#v", published)
	}
	if err := reopened.step(context.Background(), publishSnapshot, publish, publishUnchanged); err != nil {
		t.Fatal(err)
	}
	if len(published) != 2 || published[1].ObservationID != published[0].ObservationID {
		t.Fatalf("pending observation was not retried: %#v", published)
	}
	if err := reopened.Ack(published[0].ObservationID); err != nil {
		t.Fatal(err)
	}
	if err := reopened.step(context.Background(), publishSnapshot, publish, publishUnchanged); err != nil {
		t.Fatal(err)
	}
	if len(published) != 3 || published[2].PreviousAddress != "8.8.4.4" || published[2].Address != "1.1.1.1" {
		t.Fatalf("next change = %#v", published)
	}
}

func TestMonitorPersistsFastUnchangedReconciliation(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "ip-state.json")
	observer := &sequenceObserver{values: []string{"8.8.8.8"}}
	monitor, err := OpenMonitor(filePath, observer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	monitor.now = func() time.Time { return now }
	noObservation := func(protocol.IPObservationBody) error { return nil }
	snapshots := []protocol.IPSnapshotBody{}
	publishSnapshot := func(event protocol.IPSnapshotBody) error {
		snapshots = append(snapshots, event)
		return nil
	}
	unchanged := []protocol.ChangeIPUnchangedBody{}
	publishUnchanged := func(event protocol.ChangeIPUnchangedBody) error {
		unchanged = append(unchanged, event)
		return nil
	}
	if err := monitor.step(context.Background(), publishSnapshot, noObservation, publishUnchanged); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("initial snapshots = %#v", snapshots)
	}
	if err := monitor.AckSnapshot(snapshots[0].SnapshotID); err != nil {
		t.Fatal(err)
	}
	commandID := "123e4567-e89b-42d3-a456-426614174000"
	if err := monitor.ArmChange(commandID, "8.8.8.8", now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	for index := 0; index < 3; index++ {
		if err := monitor.step(context.Background(), publishSnapshot, noObservation, publishUnchanged); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	if len(unchanged) != 1 || unchanged[0].CommandID != commandID || unchanged[0].Address != "8.8.8.8" {
		t.Fatalf("unchanged events = %#v", unchanged)
	}
	reopened, err := OpenMonitor(filePath, observer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.AckUnchanged(commandID); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorArmsBeforeInitialCycleAndObservesChangedAddress(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "ip-state.json")
	observer := &sequenceObserver{values: []string{"8.8.4.4"}}
	monitor, err := OpenMonitor(filePath, observer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	monitor.now = func() time.Time { return now }
	commandID := "123e4567-e89b-42d3-a456-426614174000"
	if err := monitor.ArmChange(commandID, "8.8.8.8", now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(46 * time.Minute)
	observed := []protocol.IPObservationBody{}
	unchanged := []protocol.ChangeIPUnchangedBody{}
	if err := monitor.step(
		context.Background(),
		func(protocol.IPSnapshotBody) error { return nil },
		func(event protocol.IPObservationBody) error {
			observed = append(observed, event)
			return nil
		},
		func(event protocol.ChangeIPUnchangedBody) error {
			unchanged = append(unchanged, event)
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	if len(unchanged) != 0 {
		t.Fatalf("changed address produced an unchanged result: %#v", unchanged)
	}
	if len(observed) != 1 || observed[0].PreviousAddress != "8.8.8.8" || observed[0].Address != "8.8.4.4" {
		t.Fatalf("observed events = %#v", observed)
	}
}

func TestCheckIdleIncludesPendingIPState(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "ip-state.json")
	monitor, err := OpenMonitor(
		filePath, &sequenceObserver{values: []string{"8.8.8.8"}}, time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckIdle(filePath); err != nil {
		t.Fatalf("empty IP state is not idle: %v", err)
	}
	if err := monitor.ArmChange(
		"123e4567-e89b-42d3-a456-426614174000", "8.8.8.8", time.Now(),
	); err != nil {
		t.Fatal(err)
	}
	if err := CheckIdle(filePath); err == nil {
		t.Fatal("CheckIdle accepted pending ChangeIP reconciliation")
	}
}

func TestOpenMonitorRejectsIntervalsLongerThanFiveMinutes(t *testing.T) {
	_, err := OpenMonitor(
		filepath.Join(t.TempDir(), "ip-state.json"),
		&sequenceObserver{values: []string{"8.8.8.8"}}, 5*time.Minute+time.Second,
	)
	if err == nil {
		t.Fatal("OpenMonitor accepted an interval longer than five minutes")
	}
}
