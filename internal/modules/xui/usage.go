package xui

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/xray/ledger"
)

const (
	usageInterval = time.Minute
	// More clients than this would not fit one protocol message; the report
	// then says the counts are unavailable rather than send part of them.
	maxUsageClients = 4096
)

// Usage reports each minute what the owned clients' connections carried,
// counted from the kernel by the connection ledger. Only the newest report
// matters, so an unacknowledged one is replaced, never queued.
type Usage struct {
	usage func() ledger.Usage
	wake  chan struct{}

	mu      sync.Mutex
	pending *protocol.ReportBody
}

var _ module.Reporter = (*Usage)(nil)

func NewUsage(usage func() ledger.Usage) *Usage {
	return &Usage{usage: usage, wake: make(chan struct{}, 1)}
}

type usageReport struct {
	Epoch     string        `json:"epoch"`
	ErrorCode string        `json:"error_code"`
	Clients   []usageClient `json:"clients"`
}

type usageClient struct {
	Email       string `json:"email"`
	Up          uint64 `json:"up"`
	Down        uint64 `json:"down"`
	Connections int    `json:"connections"`
}

func (u *Usage) Run(ctx context.Context, publish module.Publish) error {
	ticker := time.NewTicker(usageInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-u.wake:
		}
		u.report(publish)
	}
}

func (u *Usage) report(publish module.Publish) {
	usage := u.usage()
	r := usageReport{Epoch: usage.Epoch, Clients: []usageClient{}}
	for _, t := range usage.Users {
		if Owned(t.Email) {
			r.Clients = append(r.Clients, usageClient{Email: t.Email, Up: t.Up, Down: t.Down, Connections: t.Connections})
		}
	}
	switch {
	case usage.Kernel != nil:
		r.ErrorCode = "xui_usage_unavailable"
	case usage.Log != nil:
		r.ErrorCode = "xui_log_unavailable"
	}
	if len(r.Clients) > maxUsageClients {
		r.ErrorCode, r.Clients = "xui_usage_unavailable", []usageClient{}
	}
	data, err := json.Marshal(r)
	if err != nil {
		return
	}
	u.mu.Lock()
	u.pending = &protocol.ReportBody{ReportID: protocol.NewUUID(), Kind: KindUsage, Data: json.RawMessage(data)}
	body := *u.pending
	u.mu.Unlock()
	// Without a session the next report or ready session sends it.
	_ = publish(body)
}

func (u *Usage) ControlReady() {
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

func (u *Usage) Acknowledge(reportID string) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.pending == nil || u.pending.ReportID != reportID {
		return false, nil
	}
	u.pending = nil
	return true, nil
}

func (u *Usage) UpdateSafe() error { return nil }
