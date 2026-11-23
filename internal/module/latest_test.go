package module

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

// The report goes out with the sample as a JSON object. Cloud closes the
// connection on anything else, so a node with such a reporter on would never
// stay connected (metrics in v1.29.0 sent the bytes as a base64 string).
func TestPublishedSampleIsAnObject(t *testing.T) {
	l := NewLatest("metrics.sample", time.Minute, func(context.Context) ([]byte, error) {
		return []byte(`{"boot_id":"52784529-8786-4cc3-8a3d-16085a0627bd"}`), nil
	})
	var sent []protocol.ReportBody
	l.tick(context.Background(), func(r protocol.ReportBody) error { sent = append(sent, r); return nil })
	if len(sent) != 1 {
		t.Fatalf("sent %d reports, want 1", len(sent))
	}
	wire, err := json.Marshal(sent[0])
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Kind string         `json:"kind"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(wire, &got); err != nil || got.Kind != "metrics.sample" || got.Data["boot_id"] == nil {
		t.Fatalf("report on the wire = %s (%v)", wire, err)
	}
}

// Each sample is a new report once the last is acknowledged, even when
// nothing changed, since Cloud reads speed from two snapshots in a row.
// Until then the same sample goes again under its own id; a different one
// replaces it.
func TestLatestSendsTheNewestUntilAcknowledged(t *testing.T) {
	data, failing := `{"n":1}`, false
	l := NewLatest("xui.snapshot", time.Minute, func(context.Context) ([]byte, error) {
		if failing {
			return nil, errors.New("unreadable")
		}
		return []byte(data), nil
	})
	var sent []protocol.ReportBody
	tick := func() {
		l.tick(context.Background(), func(r protocol.ReportBody) error { sent = append(sent, r); return nil })
	}

	tick()
	tick()
	if sent[1].ReportID != sent[0].ReportID {
		t.Fatal("an unacknowledged report was replaced by an identical one")
	}
	data = `{"n":2}`
	tick()
	if sent[2].ReportID == sent[1].ReportID {
		t.Fatal("a changed sample kept the unacknowledged report's id")
	}
	if ok, _ := l.Acknowledge(sent[0].ReportID); ok {
		t.Fatal("a replaced report was acknowledged")
	}
	if ok, _ := l.Acknowledge(sent[2].ReportID); !ok {
		t.Fatal("acknowledgement not taken")
	}
	tick()
	if sent[3].ReportID == sent[2].ReportID {
		t.Fatal("a quiet minute sent no new report")
	}
	failing = true
	tick()
	if len(sent) != 4 {
		t.Fatalf("a failed sample sent %d reports", len(sent)-4)
	}
}
