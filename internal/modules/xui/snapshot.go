package xui

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const (
	snapshotInterval  = time.Minute
	snapshotHeartbeat = 10 * time.Minute
	// Leaves room for the envelope within protocol.MaxMessage.
	maxSnapshotBytes = 900 * 1024
)

// Snapshots reports the panel's inbounds and the owned clients' traffic each
// minute when they changed, and at least every ten minutes. Only the newest
// snapshot matters, so an unacknowledged one is replaced, never queued.
type Snapshots struct {
	panel *Panel
	wake  chan struct{}

	mu       sync.Mutex
	pending  *protocol.ReportBody
	pendData []byte
	lastData []byte
	lastAt   time.Time
}

var _ module.Reporter = (*Snapshots)(nil)

func NewSnapshots(panel *Panel) *Snapshots {
	return &Snapshots{panel: panel, wake: make(chan struct{}, 1)}
}

type snapshot struct {
	Inbounds []snapshotInbound `json:"inbounds"`
	Traffic  []clientTraffic   `json:"traffic"`
}

type snapshotInbound struct {
	ID             int             `json:"id"`
	Remark         string          `json:"remark"`
	Protocol       string          `json:"protocol"`
	Listen         string          `json:"listen"`
	Port           int             `json:"port"`
	Enable         bool            `json:"enable"`
	Settings       json.RawMessage `json:"settings"`
	StreamSettings json.RawMessage `json:"stream_settings"`
}

type clientTraffic struct {
	Email string `json:"email"`
	Up    int64  `json:"up"`
	Down  int64  `json:"down"`
	// ResetSeq is the last reset this node executed; Cloud ignores counts
	// from before the reset it currently wants.
	ResetSeq int64 `json:"reset_seq"`
}

func (s *Snapshots) Run(ctx context.Context, publish module.Publish) error {
	ticker := time.NewTicker(snapshotInterval)
	defer ticker.Stop()
	force := true
	for {
		s.tick(ctx, publish, force)
		force = false
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-s.wake:
			force = true
		}
	}
}

func (s *Snapshots) tick(ctx context.Context, publish module.Publish, force bool) {
	readContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	data, err := s.read(readContext)
	cancel()
	if err != nil {
		slog.Warn("3x-ui snapshot unavailable", "code", err.Error())
		return
	}
	s.mu.Lock()
	switch {
	case s.pending != nil && bytes.Equal(s.pendData, data):
	case force || s.pending != nil || !bytes.Equal(s.lastData, data) || time.Since(s.lastAt) >= snapshotHeartbeat:
		s.pending = &protocol.ReportBody{ReportID: protocol.NewUUID(), Kind: KindSnapshot, Data: json.RawMessage(data)}
		s.pendData = data
	default:
		s.mu.Unlock()
		return
	}
	report := *s.pending
	s.mu.Unlock()
	// Without a session the next tick or ready session sends it.
	_ = publish(report)
}

func (s *Snapshots) read(ctx context.Context) ([]byte, error) {
	s.panel.mu.Lock()
	defer s.panel.mu.Unlock()
	inbounds, err := s.panel.adapter.list(ctx)
	if err != nil {
		return nil, err
	}
	result := snapshot{Inbounds: []snapshotInbound{}, Traffic: []clientTraffic{}}
	for _, ib := range inbounds {
		settings, stream := publicConfig(ib)
		result.Inbounds = append(result.Inbounds, snapshotInbound{
			ID: ib.ID, Remark: ib.Remark, Protocol: ib.Protocol, Listen: ib.Listen, Port: ib.Port,
			Enable: ib.Enable, Settings: settings, StreamSettings: stream,
		})
		for _, stat := range ib.ClientStats {
			if strings.HasPrefix(stat.Email, ownedPrefix) {
				result.Traffic = append(result.Traffic, clientTraffic{
					Email: stat.Email, Up: stat.Up, Down: stat.Down, ResetSeq: s.panel.resets.get(stat.Email),
				})
			}
		}
	}
	data, err := json.Marshal(result)
	if err != nil || len(data) > maxSnapshotBytes {
		return nil, errorCode("xui_snapshot_too_large")
	}
	return data, nil
}

// publicConfig keeps what a client needs to connect to an inbound Cloud can
// deliver, and only network and security of any other. Clients (other users'
// credentials), server private keys, the VLESS decryption key, Reality seeds
// and inline certificates never leave the node.
func publicConfig(ib inbound) (json.RawMessage, json.RawMessage) {
	var settings, stream map[string]any
	_ = json.Unmarshal([]byte(ib.Settings), &settings)
	_ = json.Unmarshal([]byte(ib.StreamSettings), &stream)
	switch ib.Protocol {
	case "vless", "shadowsocks", "hysteria", "hysteria2":
		delete(settings, "clients")
		delete(settings, "decryption")
		delete(settings, "fallbacks")
		if reality, ok := stream["realitySettings"].(map[string]any); ok {
			delete(reality, "privateKey")
			delete(reality, "mldsa65Seed")
		}
		if tls, ok := stream["tlsSettings"].(map[string]any); ok {
			delete(tls, "certificates")
		}
	default:
		settings = map[string]any{}
		stream = map[string]any{"network": stream["network"], "security": stream["security"]}
	}
	if settings == nil {
		settings = map[string]any{}
	}
	if stream == nil {
		stream = map[string]any{}
	}
	s, _ := json.Marshal(settings)
	t, _ := json.Marshal(stream)
	return s, t
}

type errorCode string

func (e errorCode) Error() string { return string(e) }

func (s *Snapshots) ControlReady() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Snapshots) Acknowledge(reportID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil || s.pending.ReportID != reportID {
		return false, nil
	}
	s.lastData, s.lastAt = s.pendData, time.Now()
	s.pending, s.pendData = nil, nil
	return true, nil
}

func (s *Snapshots) UpdateSafe() error { return nil }
