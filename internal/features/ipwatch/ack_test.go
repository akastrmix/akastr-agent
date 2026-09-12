package ipwatch

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

func TestAcknowledgementReplayPreservesNextEventAndWakesOnlyOnce(t *testing.T) {
	for _, kind := range []string{"snapshot4", "snapshot6", "observation4", "observation6", "unchanged"} {
		t.Run(kind, func(t *testing.T) {
			m, err := OpenMonitor(filepath.Join(t.TempDir(), "ip.json"), &sequenceObserver{values: []string{"8.8.8.8"}}, time.Minute, true)
			if err != nil {
				t.Fatal(err)
			}
			id, nextID := protocol.NewUUID(), protocol.NewUUID()
			var ack func(string) error
			var setPending func(string)
			wake := m.wake
			switch kind {
			case "snapshot4":
				ack = m.AckSnapshot
				setPending = func(id string) { m.snapshot.PendingSnapshot = &protocol.IPSnapshotBody{SnapshotID: id} }
			case "snapshot6":
				ack, wake = m.AckSnapshot, m.wakeIPv6
				setPending = func(id string) { m.snapshot.PendingIPv6Snapshot = &protocol.IPSnapshotBody{SnapshotID: id} }
			case "observation4":
				ack = m.Ack
				setPending = func(id string) { m.snapshot.Pending = &protocol.IPObservationBody{ObservationID: id} }
			case "observation6":
				ack, wake = m.Ack, m.wakeIPv6
				setPending = func(id string) { m.snapshot.PendingIPv6 = &protocol.IPObservationBody{ObservationID: id} }
			case "unchanged":
				ack = m.AckUnchanged
				setPending = func(id string) { m.snapshot.PendingUnchanged = &protocol.ChangeIPUnchangedBody{CommandID: id} }
			}
			setPending(id)
			if err := ack(id); err != nil {
				t.Fatal(err)
			}
			select {
			case <-wake:
			default:
				t.Fatal("confirmed event did not wake its observer")
			}
			setPending(nextID)
			if err := ack(id); err != nil {
				t.Fatal(err)
			}
			select {
			case <-wake:
				t.Fatal("duplicate acknowledgement woke the observer")
			default:
			}
			if err := ack(nextID); err != nil {
				t.Fatal(err)
			}
			select {
			case <-wake:
			default:
				t.Fatal("duplicate acknowledgement removed the new event")
			}
		})
	}
}
