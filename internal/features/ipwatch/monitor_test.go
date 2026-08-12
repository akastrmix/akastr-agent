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
	publish := func(event protocol.IPObservationBody) error {
		published = append(published, event)
		return nil
	}
	if err := monitor.step(context.Background(), publish); err != nil {
		t.Fatal(err)
	}
	if len(published) != 0 {
		t.Fatal("initial observation must not be announced as a change")
	}
	if err := monitor.step(context.Background(), publish); err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 || published[0].PreviousAddress != "8.8.8.8" || published[0].Address != "8.8.4.4" {
		t.Fatalf("published = %#v", published)
	}
	if err := monitor.step(context.Background(), publish); err != nil {
		t.Fatal(err)
	}
	if len(published) != 2 || published[1].ObservationID != published[0].ObservationID {
		t.Fatalf("pending observation was not retried: %#v", published)
	}
	reopened, err := OpenMonitor(filePath, observer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Ack(published[0].ObservationID); err != nil {
		t.Fatal(err)
	}
	if err := reopened.step(context.Background(), publish); err != nil {
		t.Fatal(err)
	}
	if len(published) != 3 || published[2].PreviousAddress != "8.8.4.4" || published[2].Address != "1.1.1.1" {
		t.Fatalf("next change = %#v", published)
	}
}
