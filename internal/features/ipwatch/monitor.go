package ipwatch

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/state"
)

type AddressObserver interface {
	Observe(context.Context, Family) (Observation, error)
}

type monitorSnapshot struct {
	SchemaVersion int                         `json:"schema_version"`
	LastIPv4      string                      `json:"last_ipv4,omitempty"`
	Pending       *protocol.IPObservationBody `json:"pending,omitempty"`
}

type Monitor struct {
	mu       sync.Mutex
	file     *state.JSONFile
	observer AddressObserver
	interval time.Duration
	snapshot monitorSnapshot
}

var errTransientMonitor = errors.New("transient IP monitor failure")

func OpenMonitor(filePath string, observer AddressObserver, interval time.Duration) (*Monitor, error) {
	if observer == nil || interval < 10*time.Second || interval > time.Hour {
		return nil, errors.New("IP monitor options are invalid")
	}
	monitor := &Monitor{
		file: state.NewJSONFile(filePath), observer: observer, interval: interval,
		snapshot: monitorSnapshot{SchemaVersion: 1},
	}
	found, err := monitor.file.Load(&monitor.snapshot)
	if err != nil {
		return nil, err
	}
	if found {
		if monitor.snapshot.SchemaVersion != 1 {
			return nil, errors.New("IP state schema is unsupported")
		}
		if monitor.snapshot.LastIPv4 != "" {
			address, parseError := netip.ParseAddr(monitor.snapshot.LastIPv4)
			if parseError != nil || !address.Is4() {
				return nil, errors.New("IP state last_ipv4 is invalid")
			}
		}
		if pending := monitor.snapshot.Pending; pending != nil {
			if pending.Family != "ipv4" || pending.PreviousAddress == pending.Address || pending.ObservationID == "" {
				return nil, errors.New("IP state pending observation is invalid")
			}
		}
	}
	return monitor, nil
}

func (m *Monitor) Run(ctx context.Context, publish func(protocol.IPObservationBody) error) error {
	if publish == nil {
		return errors.New("IP observation publisher is required")
	}
	for {
		if err := m.step(ctx, publish); err != nil && ctx.Err() == nil {
			if !errors.Is(err, errTransientMonitor) {
				return err
			}
		}
		timer := time.NewTimer(m.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Monitor) Ack(observationID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.Pending == nil || m.snapshot.Pending.ObservationID != observationID {
		return errors.New("IP observation acknowledgment does not match pending state")
	}
	next := m.snapshot
	next.Pending = nil
	if err := m.file.Save(next); err != nil {
		return err
	}
	m.snapshot = next
	return nil
}

func (m *Monitor) step(ctx context.Context, publish func(protocol.IPObservationBody) error) error {
	m.mu.Lock()
	if m.snapshot.Pending != nil {
		pending := *m.snapshot.Pending
		m.mu.Unlock()
		if err := publish(pending); err != nil {
			return fmt.Errorf("%w: publish pending observation", errTransientMonitor)
		}
		return nil
	}
	m.mu.Unlock()
	observation, err := m.observer.Observe(ctx, IPv4)
	if err != nil {
		return fmt.Errorf("%w: observe IPv4", errTransientMonitor)
	}
	current := observation.Address.String()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.LastIPv4 == "" {
		next := m.snapshot
		next.LastIPv4 = current
		if err := m.file.Save(next); err != nil {
			return err
		}
		m.snapshot = next
		return nil
	}
	if m.snapshot.LastIPv4 == current {
		return nil
	}
	pending := &protocol.IPObservationBody{
		ObservationID: protocol.NewUUID(), Family: "ipv4",
		PreviousAddress: m.snapshot.LastIPv4, Address: current,
		ObservedAt: observation.ObservedAt.UTC().Format(time.RFC3339Nano),
	}
	next := m.snapshot
	next.LastIPv4 = current
	next.Pending = pending
	if err := m.file.Save(next); err != nil {
		return fmt.Errorf("persist IP observation: %w", err)
	}
	m.snapshot = next
	if err := publish(*pending); err != nil {
		return fmt.Errorf("%w: publish observation", errTransientMonitor)
	}
	return nil
}
