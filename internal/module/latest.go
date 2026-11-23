package module

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

// Latest is a Reporter for facts of which only the newest matters, such as a
// snapshot each minute. It samples when it starts, every interval and
// whenever a session becomes ready; a failed sample sends nothing. An
// unacknowledged report is replaced by a sample that says something else,
// never queued, and sent again as it is while the samples say the same.
type Latest struct {
	kind     string
	interval time.Duration
	sample   func(context.Context) ([]byte, error)
	wake     chan struct{}

	mu      sync.Mutex
	pending *protocol.ReportBody
	data    []byte
}

var _ Reporter = (*Latest)(nil)

// NewLatest reports as kind the JSON value sample returns.
func NewLatest(kind string, interval time.Duration, sample func(context.Context) ([]byte, error)) *Latest {
	return &Latest{kind: kind, interval: interval, sample: sample, wake: make(chan struct{}, 1)}
}

func (l *Latest) Run(ctx context.Context, publish Publish) error {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		l.tick(ctx, publish)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-l.wake:
		}
	}
}

func (l *Latest) tick(ctx context.Context, publish Publish) {
	data, err := l.sample(ctx)
	if err != nil {
		return
	}
	// The report is stored before it is sent: an acknowledgement arriving
	// first would match nothing.
	l.mu.Lock()
	if l.pending == nil || !bytes.Equal(l.data, data) {
		// Plain bytes would go out as a base64 string, which Cloud refuses by
		// closing the connection.
		l.pending = &protocol.ReportBody{ReportID: protocol.NewUUID(), Kind: l.kind, Data: json.RawMessage(data)}
		l.data = data
	}
	report := *l.pending
	l.mu.Unlock()
	// Without a session the next tick or ready session sends it.
	_ = publish(report)
}

func (l *Latest) ControlReady() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *Latest) Acknowledge(reportID string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending == nil || l.pending.ReportID != reportID {
		return false, nil
	}
	l.pending, l.data = nil, nil
	return true, nil
}

func (l *Latest) UpdateSafe() error { return nil }
