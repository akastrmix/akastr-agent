package routing

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/panel"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const (
	// KindHealth reports whether each owned outbound reaches the panel's
	// outbound test URL.
	KindHealth     = "routing.outbound_health"
	healthInterval = 5 * time.Minute
	// 3x-ui 2.9.4 words a test whose request through the outbound got no
	// answer this way; its other failures (another test running, the
	// temporary Xray not starting) say nothing about the outbound.
	requestFailed = "Request failed"
)

// HealthPanel is what the outbound check uses of the panel.
type HealthPanel interface {
	Template(context.Context) (panel.Template, error)
	TestOutbound(ctx context.Context, outbound, all json.RawMessage) (panel.OutboundTest, error)
}

// Health tests every owned outbound in the panel's template every five
// minutes with the panel's own outbound test, one at a time, and reports
// each as up with its delay, down, or untested. The panel's error text is
// neither reported nor logged: it can name the outbound's server. Only the
// newest round matters, so an unacknowledged report is replaced, never queued.
type Health struct {
	panel HealthPanel
	wake  chan struct{}

	mu      sync.Mutex
	pending *protocol.ReportBody
}

var _ module.Reporter = (*Health)(nil)

func NewHealth(p HealthPanel) *Health {
	return &Health{panel: p, wake: make(chan struct{}, 1)}
}

type outboundHealth struct {
	Tag string `json:"tag"`
	// Up is null when the panel could not run the test.
	Up      *bool  `json:"up"`
	DelayMS *int64 `json:"delay_ms"`
}

func (h *Health) Run(ctx context.Context, publish module.Publish) error {
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	for {
		h.round(ctx, publish)
		if err := h.waitRound(ctx, ticker.C, publish); err != nil {
			return err
		}
	}
}

// waitRound resends the pending report whenever a session becomes ready
// until the next round is due.
func (h *Health) waitRound(ctx context.Context, tick <-chan time.Time, publish module.Publish) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick:
			return nil
		case <-h.wake:
			h.send(publish)
		}
	}
}

func (h *Health) round(ctx context.Context, publish module.Publish) {
	results, err := h.test(ctx)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		slog.Warn("outbound check skipped", "code", err.Error())
		return
	}
	data, _ := json.Marshal(struct {
		Outbounds []outboundHealth `json:"outbounds"`
	}{results})
	h.mu.Lock()
	h.pending = &protocol.ReportBody{ReportID: protocol.NewUUID(), Kind: KindHealth, Data: json.RawMessage(data)}
	h.mu.Unlock()
	h.send(publish)
}

// test runs the panel's test on each owned outbound of the template, with all
// of the template's outbounds alongside as the panel's own page sends them.
func (h *Health) test(ctx context.Context) ([]outboundHealth, error) {
	template, err := h.panel.Template(ctx)
	if err != nil {
		return nil, err
	}
	all := template.Config["outbounds"]
	var outbounds []json.RawMessage
	if err := json.Unmarshal(all, &outbounds); err != nil {
		return nil, errorCode("routing_template_invalid")
	}
	results := []outboundHealth{}
	for _, raw := range outbounds {
		var o struct {
			Tag string `json:"tag"`
		}
		if json.Unmarshal(raw, &o) != nil || !ownedTag.MatchString(o.Tag) {
			continue
		}
		result := outboundHealth{Tag: o.Tag}
		found, err := h.panel.TestOutbound(ctx, raw, all)
		switch {
		case err == nil && found.Success:
			result.Up, result.DelayMS = new(true), new(found.Delay)
		case err == nil && strings.HasPrefix(found.Error, requestFailed):
			result.Up = new(false)
		case err == nil:
			slog.Warn("outbound test unavailable", "tag", o.Tag, "code", "outbound_test_unavailable")
		default:
			slog.Warn("outbound test unavailable", "tag", o.Tag, "code", err.Error())
		}
		results = append(results, result)
	}
	return results, nil
}

func (h *Health) send(publish module.Publish) {
	h.mu.Lock()
	report := h.pending
	h.mu.Unlock()
	// Without a session the next ready session or round sends it.
	if report != nil {
		_ = publish(*report)
	}
}

func (h *Health) ControlReady() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *Health) Acknowledge(reportID string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending == nil || h.pending.ReportID != reportID {
		return false, nil
	}
	h.pending = nil
	return true, nil
}

func (h *Health) UpdateSafe() error { return nil }

type errorCode string

func (e errorCode) Error() string { return string(e) }
