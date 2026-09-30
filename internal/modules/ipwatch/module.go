package ipwatch

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

// Module name in the node configuration.
const Name = "ip_watch"

// Report kinds this module sends.
const (
	// KindAddress says which address the node has now. CommandID names the
	// ChangeIP whose reconciliation saw the change; it is null otherwise.
	KindAddress = "ip.address"
	// KindUnchanged says a triggered ChangeIP left the IPv4 address as it was.
	KindUnchanged = "changeip.unchanged"
)

type AddressData struct {
	Family     string  `json:"family"`
	Address    string  `json:"address"`
	ObservedAt string  `json:"observed_at"`
	CommandID  *string `json:"command_id"`
}

type UnchangedData struct {
	CommandID  string `json:"command_id"`
	Address    string `json:"address"`
	ObservedAt string `json:"observed_at"`
}

type Config struct {
	IntervalSeconds int  `json:"interval_seconds"`
	IPv6            bool `json:"ipv6"`
}

func ParseConfig(raw json.RawMessage) (Config, error) {
	cfg, err := protocol.DecodeStrict[Config](raw, Name, "interval_seconds", "ipv6")
	if err != nil {
		return Config{}, err
	}
	if cfg.IntervalSeconds < 10 || cfg.IntervalSeconds > 300 {
		return Config{}, errors.New("ip_watch interval_seconds must be between 10 and 300")
	}
	return cfg, nil
}

// Reporter exposes the monitor to the control connection.
type Reporter struct{ Monitor *Monitor }

var _ module.Reporter = Reporter{}

func (r Reporter) Run(ctx context.Context, publish module.Publish) error {
	return r.Monitor.Run(ctx, publish)
}

func (r Reporter) ControlReady() { r.Monitor.NotifyControlReady() }

func (r Reporter) Acknowledge(reportID string) (bool, error) {
	return r.Monitor.Acknowledge(reportID)
}

// UpdateSafe keeps a ChangeIP reconciliation in the process that started it.
func (r Reporter) UpdateSafe() error {
	if r.Monitor.reconciling() {
		return errors.New("ChangeIP reconciliation is pending")
	}
	return nil
}
