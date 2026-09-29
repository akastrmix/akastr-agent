package ipwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

// Module name in the node configuration.
const Name = "ip_watch"

// Messages this module sends and the acknowledgements Cloud returns.
type SnapshotBody struct {
	SnapshotID string `json:"snapshot_id"`
	Family     string `json:"family"`
	Address    string `json:"address"`
	ObservedAt string `json:"observed_at"`
}

type ObservationBody struct {
	ObservationID   string `json:"observation_id"`
	Family          string `json:"family"`
	PreviousAddress string `json:"previous_address"`
	Address         string `json:"address"`
	ObservedAt      string `json:"observed_at"`
}

type UnchangedBody struct {
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

func (c Config) Capability() capability.Descriptor {
	return capability.Descriptor{
		Name: "ip.observe", Version: 1,
		Properties: map[string]any{
			"interval_seconds": strconv.Itoa(c.IntervalSeconds),
			"observe_ipv6":     strconv.FormatBool(c.IPv6),
		},
	}
}

// Reporter exposes the monitor to the control connection.
type Reporter struct{ Monitor *Monitor }

var _ module.Reporter = Reporter{}

func (r Reporter) Run(ctx context.Context, publish module.Publish) error {
	return r.Monitor.Run(ctx,
		func(body SnapshotBody) error { return publish("ip.snapshot", body) },
		func(body ObservationBody) error { return publish("ip.observed", body) },
		func(body UnchangedBody) error { return publish("changeip.unchanged", body) },
	)
}

func (r Reporter) ControlReady() { r.Monitor.NotifyControlReady() }

func (r Reporter) Acknowledge(envelope protocol.Envelope) (bool, error) {
	var idField, id string
	var persisted bool
	switch envelope.Type {
	case "ip.snapshot_ack":
		body, err := protocol.DecodeBody[struct {
			SnapshotID string `json:"snapshot_id"`
			Persisted  bool   `json:"persisted"`
		}](envelope, "snapshot_id", "persisted")
		if err != nil {
			return true, err
		}
		idField, id, persisted = "snapshot", body.SnapshotID, body.Persisted
	case "ip.observed_ack":
		body, err := protocol.DecodeBody[struct {
			ObservationID string `json:"observation_id"`
			Persisted     bool   `json:"persisted"`
		}](envelope, "observation_id", "persisted")
		if err != nil {
			return true, err
		}
		idField, id, persisted = "observation", body.ObservationID, body.Persisted
	case "changeip.unchanged_ack":
		body, err := protocol.DecodeBody[struct {
			CommandID string `json:"command_id"`
			Persisted bool   `json:"persisted"`
		}](envelope, "command_id", "persisted")
		if err != nil {
			return true, err
		}
		idField, id, persisted = "unchanged", body.CommandID, body.Persisted
	default:
		return false, nil
	}
	if !protocol.ValidUUID(id) {
		return true, fmt.Errorf("invalid %s acknowledgement identifier", idField)
	}
	if !persisted {
		return true, fmt.Errorf("IP %s was not persisted", idField)
	}
	switch idField {
	case "snapshot":
		return true, r.Monitor.AckSnapshot(id)
	case "observation":
		return true, r.Monitor.Ack(id)
	default:
		return true, r.Monitor.AckUnchanged(id)
	}
}

// UpdateSafe keeps a ChangeIP reconciliation in the process that started it.
// Pending IP facts are durable and are replayed by the next process.
func (r Reporter) UpdateSafe() error {
	r.Monitor.mu.Lock()
	defer r.Monitor.mu.Unlock()
	if r.Monitor.snapshot.ChangeAttempt != nil || r.Monitor.snapshot.PendingUnchanged != nil {
		return errors.New("ChangeIP reconciliation is pending")
	}
	return nil
}
