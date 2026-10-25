// Package connectionrecords reports to Cloud a record of each request the
// access log names for an owned client, and once a bound TCP connection ends,
// the same record again with when it ended and what it carried. Cloud merges
// the two by the line's time and source, so a record sent twice changes
// nothing.
//
// Records wait in memory only, up to maxQueued; beyond that the oldest are
// dropped and the next batch says how many. An update or restart loses what
// was not sent yet, usually less than one batch interval. Lines read on
// opening the log were sent by an earlier process: they only help the ledger
// find the connections already open, whose end records still come.
package connectionrecords

import (
	"context"
	"encoding/json"
	"net/netip"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/modules/xui"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/xray/accesslog"
	"github.com/akastrmix/akastr-agent/internal/xray/ledger"
)

const (
	Name      = "connection_records"
	KindBatch = "connection_records.batch"

	batchInterval = 10 * time.Second
	maxBatch      = 1000
	// About two hours of the busiest node.
	maxQueued = 20000
	// Bounds a record so that a full batch fits one protocol message; Xray
	// configurations name outbounds briefly.
	maxOutbound = 128
	timeLayout  = "2006-01-02T15:04:05.000000Z"
)

// Config has no fields: the module is on or off.
type Config struct{}

func ParseConfig(raw json.RawMessage) (Config, error) {
	return protocol.DecodeStrict[Config](raw, Name)
}

type record struct {
	At       string `json:"at"`
	Email    string `json:"email"`
	Source   string `json:"source"`
	Network  string `json:"network"`
	Host     string `json:"host"`
	Port     uint16 `json:"port"`
	Outbound string `json:"outbound"`
	Blocked  bool   `json:"blocked"`
	*ending
}

// ending is what an end record adds: all four fields or none.
type ending struct {
	EndedAt  string `json:"ended_at"`
	Up       uint64 `json:"up"`
	Down     uint64 `json:"down"`
	Requests int    `json:"requests"`
}

type batch struct {
	Records []record `json:"records"`
	Dropped int      `json:"dropped"`
}

// Records queues the records and sends them in batches, one batch waiting
// for its acknowledgement at a time.
type Records struct {
	wake chan struct{}

	mu      sync.Mutex
	queue   []record
	dropped int
	pending *protocol.ReportBody
}

var _ module.Reporter = (*Records)(nil)

func New() *Records { return &Records{wake: make(chan struct{}, 1)} }

// Lines takes in the access log's lines.
func (r *Records) Lines(lines []accesslog.Line) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range lines {
		if !line.Backlog {
			r.add(line, nil)
		}
	}
}

// Ended takes in a bound connection that ended.
func (r *Records) Ended(e ledger.Ended) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.add(e.Line, &ending{EndedAt: e.At.UTC().Format(timeLayout), Up: e.Up, Down: e.Down, Requests: e.Requests})
}

// add queues the record of a line of an owned client, which only a line
// with its time, destination and outbound can make. Cloud refuses a batch
// with one record it cannot read, and the batch would be sent again forever.
func (r *Records) add(line accesslog.Line, end *ending) {
	if !xui.Owned(line.Email) || line.Written.IsZero() || line.Dest.Network == "" || line.Outbound == "" || len(line.Outbound) > maxOutbound {
		return
	}
	source := netip.AddrPortFrom(line.Conn.Remote.Addr().WithZone(""), line.Conn.Remote.Port())
	r.queue = append(r.queue, record{At: line.Written.UTC().Format(timeLayout), Email: line.Email, Source: source.String(),
		Network: line.Dest.Network, Host: line.Dest.Host, Port: line.Dest.Port, Outbound: line.Outbound, Blocked: line.Blocked, ending: end})
	if len(r.queue) > maxQueued {
		// Dropping a tenth at once keeps a flood from copying the queue on
		// every line.
		drop := len(r.queue) - maxQueued*9/10
		r.queue = append(r.queue[:0], r.queue[drop:]...)
		r.dropped += drop
	}
	if len(r.queue) >= maxBatch && r.pending == nil {
		r.signal()
	}
}

func (r *Records) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Records) Run(ctx context.Context, publish module.Publish) error {
	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-r.wake:
		}
		r.send(publish)
	}
}

// send sends the waiting batch again, or the next one.
func (r *Records) send(publish module.Publish) {
	r.mu.Lock()
	if r.pending == nil && (len(r.queue) > 0 || r.dropped > 0) {
		n := min(len(r.queue), maxBatch)
		data, err := json.Marshal(batch{Records: append([]record{}, r.queue[:n]...), Dropped: r.dropped})
		if err != nil {
			r.mu.Unlock()
			return
		}
		r.pending = &protocol.ReportBody{ReportID: protocol.NewUUID(), Kind: KindBatch, Data: json.RawMessage(data)}
		r.queue = append(r.queue[:0], r.queue[n:]...)
		r.dropped = 0
	}
	if r.pending == nil {
		r.mu.Unlock()
		return
	}
	body := *r.pending
	r.mu.Unlock()
	// Without a session the next tick or ready session sends it again.
	_ = publish(body)
}

func (r *Records) ControlReady() { r.signal() }

func (r *Records) Acknowledge(reportID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil || r.pending.ReportID != reportID {
		return false, nil
	}
	r.pending = nil
	if len(r.queue) >= maxBatch {
		r.signal()
	}
	return true, nil
}

// UpdateSafe allows a replacement at any time: records waiting are lost.
func (r *Records) UpdateSafe() error { return nil }
