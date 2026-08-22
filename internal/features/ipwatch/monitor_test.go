package ipwatch

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type familySequenceObserver struct {
	v4      []string
	v6      []string
	v4Index int
	v6Index int
	v6Err   error
}

func (o *familySequenceObserver) Observe(_ context.Context, family Family) (Observation, error) {
	if family == IPv6 && o.v6Err != nil {
		return Observation{}, o.v6Err
	}
	values, index := o.v4, &o.v4Index
	if family == IPv6 {
		values, index = o.v6, &o.v6Index
	}
	value := values[*index]
	if *index < len(values)-1 {
		(*index)++
	}
	return Observation{
		Address:    netip.MustParseAddr(value),
		ObservedAt: time.Date(2026, 8, 22, 0, 0, *index, 0, time.UTC),
	}, nil
}

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
	monitor, err := OpenMonitor(filePath, observer, time.Minute, false)
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
	if monitor.SnapshotReady() {
		t.Fatal("monitor became ChangeIP-ready before snapshot acknowledgement")
	}
	if err := monitor.step(context.Background(), publishSnapshot, publish, publishUnchanged); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || snapshots[1].SnapshotID != snapshots[0].SnapshotID {
		t.Fatalf("pending snapshot was not retried: %#v", snapshots)
	}
	reopened, err := OpenMonitor(filePath, observer, time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.AckSnapshot(snapshots[0].SnapshotID); err != nil {
		t.Fatal(err)
	}
	if !reopened.SnapshotReady() {
		t.Fatal("monitor did not become ChangeIP-ready after snapshot acknowledgement")
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

func TestControlReadinessWakesPendingSnapshotImmediately(t *testing.T) {
	monitor, err := OpenMonitor(
		filepath.Join(t.TempDir(), "ip-state.json"),
		&sequenceObserver{values: []string{"8.8.8.8"}}, time.Minute, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	firstAttempt := make(chan struct{})
	retried := make(chan struct{})
	attempts := 0
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- monitor.Run(
			ctx,
			func(protocol.IPSnapshotBody) error {
				attempts++
				if attempts == 1 {
					close(firstAttempt)
					return context.DeadlineExceeded
				}
				close(retried)
				return nil
			},
			func(protocol.IPObservationBody) error { return nil },
			func(protocol.ChangeIPUnchangedBody) error { return nil },
		)
	}()
	select {
	case <-firstAttempt:
	case <-time.After(time.Second):
		t.Fatal("initial snapshot publish did not run")
	}
	monitor.NotifyControlReady()
	select {
	case <-retried:
	case <-time.After(time.Second):
		t.Fatal("control readiness did not wake the pending snapshot")
	}
	cancel()
	<-done
}

func TestMonitorPersistsFastUnchangedReconciliation(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "ip-state.json")
	observer := &sequenceObserver{values: []string{"8.8.8.8"}}
	monitor, err := OpenMonitor(filePath, observer, time.Minute, false)
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
	reopened, err := OpenMonitor(filePath, observer, time.Minute, false)
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
	monitor, err := OpenMonitor(filePath, observer, time.Minute, false)
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
		filePath, &sequenceObserver{values: []string{"8.8.8.8"}}, time.Minute, false,
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
		&sequenceObserver{values: []string{"8.8.8.8"}}, 5*time.Minute+time.Second, false,
	)
	if err == nil {
		t.Fatal("OpenMonitor accepted an interval longer than five minutes")
	}
}

func TestMonitorPersistsIPv6IndependentlyFromIPv4Readiness(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "ip-state.json")
	observer := &familySequenceObserver{
		v4: []string{"8.8.8.8"},
		v6: []string{"2606:4700:4700::1111", "2001:4860:4860::8888"},
	}
	monitor, err := OpenMonitor(filePath, observer, time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	var snapshots []protocol.IPSnapshotBody
	var observations []protocol.IPObservationBody
	publishSnapshot := func(value protocol.IPSnapshotBody) error {
		snapshots = append(snapshots, value)
		return nil
	}
	publish := func(value protocol.IPObservationBody) error {
		observations = append(observations, value)
		return nil
	}
	if err := monitor.step(context.Background(), publishSnapshot, publish, func(protocol.ChangeIPUnchangedBody) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := monitor.stepIPv6(context.Background(), publishSnapshot, publish); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || snapshots[0].Family != "ipv4" || snapshots[1].Family != "ipv6" {
		t.Fatalf("snapshots = %#v", snapshots)
	}
	if err := monitor.AckSnapshot(snapshots[0].SnapshotID); err != nil {
		t.Fatal(err)
	}
	if err := CheckIdle(filePath); err != nil {
		t.Fatalf("pending IPv6 blocked operation/configuration idle state: %v", err)
	}
	if err := monitor.AckSnapshot(snapshots[1].SnapshotID); err != nil {
		t.Fatal(err)
	}
	if err := monitor.stepIPv6(context.Background(), publishSnapshot, publish); err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].Family != "ipv6" ||
		observations[0].PreviousAddress != "2606:4700:4700::1111" ||
		observations[0].Address != "2001:4860:4860::8888" {
		t.Fatalf("IPv6 observations = %#v", observations)
	}
	reopened, err := OpenMonitor(filePath, observer, time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.stepIPv6(context.Background(), publishSnapshot, publish); err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 || observations[1].ObservationID != observations[0].ObservationID {
		t.Fatalf("pending IPv6 observation was not retried: %#v", observations)
	}
}

func TestIPv6ProbeFailureIsTransient(t *testing.T) {
	monitor, err := OpenMonitor(
		filepath.Join(t.TempDir(), "ip-state.json"),
		&familySequenceObserver{v4: []string{"8.8.8.8"}, v6Err: errors.New("no IPv6 route")},
		time.Minute, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	err = monitor.stepIPv6(
		context.Background(),
		func(protocol.IPSnapshotBody) error { return nil },
		func(protocol.IPObservationBody) error { return nil },
	)
	if !errors.Is(err, errTransientMonitor) {
		t.Fatalf("IPv6 probe error = %v", err)
	}
}
