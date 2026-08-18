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
	SchemaVersion    int                             `json:"schema_version"`
	LastIPv4         string                          `json:"last_ipv4,omitempty"`
	PendingSnapshot  *protocol.IPSnapshotBody        `json:"pending_snapshot,omitempty"`
	Pending          *protocol.IPObservationBody     `json:"pending,omitempty"`
	ChangeAttempt    *changeAttempt                  `json:"change_attempt,omitempty"`
	PendingUnchanged *protocol.ChangeIPUnchangedBody `json:"pending_unchanged,omitempty"`
}

type changeAttempt struct {
	CommandID     string    `json:"command_id"`
	Address       string    `json:"address"`
	ReconcileAt   time.Time `json:"reconcile_at"`
	Confirmations int       `json:"confirmations"`
}

type Monitor struct {
	mu       sync.Mutex
	file     *state.JSONFile
	observer AddressObserver
	interval time.Duration
	now      func() time.Time
	snapshot monitorSnapshot
}

var errTransientMonitor = errors.New("transient IP monitor failure")

const (
	changeReconcileGrace         = 5 * time.Minute
	changeUnchangedConfirmations = 3
)

func OpenMonitor(filePath string, observer AddressObserver, interval time.Duration) (*Monitor, error) {
	if observer == nil || interval < 10*time.Second || interval > 5*time.Minute {
		return nil, errors.New("IP monitor options are invalid")
	}
	monitor := &Monitor{
		file: state.NewJSONFile(filePath), observer: observer, interval: interval, now: time.Now,
		snapshot: monitorSnapshot{SchemaVersion: 1},
	}
	found, err := monitor.file.Load(&monitor.snapshot)
	if err != nil {
		return nil, err
	}
	if found {
		if err := validateMonitorSnapshot(monitor.snapshot); err != nil {
			return nil, err
		}
	}
	return monitor, nil
}

func CheckIdle(filePath string) error {
	snapshot := monitorSnapshot{SchemaVersion: 1}
	found, err := state.NewJSONFile(filePath).Load(&snapshot)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if err := validateMonitorSnapshot(snapshot); err != nil {
		return err
	}
	if snapshot.PendingSnapshot != nil || snapshot.Pending != nil || snapshot.ChangeAttempt != nil || snapshot.PendingUnchanged != nil {
		return errors.New("IP observation or ChangeIP reconciliation is pending")
	}
	return nil
}

func validateMonitorSnapshot(snapshot monitorSnapshot) error {
	if snapshot.SchemaVersion != 1 {
		return errors.New("IP state schema is unsupported")
	}
	if snapshot.LastIPv4 != "" {
		address, parseError := netip.ParseAddr(snapshot.LastIPv4)
		if parseError != nil || !address.Is4() {
			return errors.New("IP state last_ipv4 is invalid")
		}
	}
	if pending := snapshot.PendingSnapshot; pending != nil {
		address, addressError := netip.ParseAddr(pending.Address)
		observedAt, timeError := time.Parse(time.RFC3339Nano, pending.ObservedAt)
		if pending.Family != "ipv4" || !protocol.ValidUUID(pending.SnapshotID) ||
			addressError != nil || !address.Is4() || timeError != nil || observedAt.IsZero() ||
			snapshot.LastIPv4 != pending.Address {
			return errors.New("IP state pending snapshot is invalid")
		}
	}
	if pending := snapshot.Pending; pending != nil {
		previous, previousError := netip.ParseAddr(pending.PreviousAddress)
		address, addressError := netip.ParseAddr(pending.Address)
		observedAt, timeError := time.Parse(time.RFC3339Nano, pending.ObservedAt)
		if pending.Family != "ipv4" || !protocol.ValidUUID(pending.ObservationID) ||
			previousError != nil || !previous.Is4() || addressError != nil || !address.Is4() ||
			previous == address || timeError != nil || observedAt.IsZero() {
			return errors.New("IP state pending observation is invalid")
		}
	}
	pendingStates := 0
	if snapshot.PendingSnapshot != nil {
		pendingStates++
	}
	if snapshot.Pending != nil {
		pendingStates++
	}
	if snapshot.ChangeAttempt != nil {
		pendingStates++
	}
	if snapshot.PendingUnchanged != nil {
		pendingStates++
	}
	if pendingStates > 1 {
		return errors.New("IP state contains multiple pending events")
	}
	if attempt := snapshot.ChangeAttempt; attempt != nil {
		address, parseError := netip.ParseAddr(attempt.Address)
		if !protocol.ValidUUID(attempt.CommandID) || parseError != nil || !address.Is4() ||
			attempt.ReconcileAt.IsZero() || attempt.Confirmations < 0 ||
			attempt.Confirmations >= changeUnchangedConfirmations {
			return errors.New("IP state ChangeIP attempt is invalid")
		}
	}
	if pending := snapshot.PendingUnchanged; pending != nil {
		address, parseError := netip.ParseAddr(pending.Address)
		observedAt, timeError := time.Parse(time.RFC3339Nano, pending.ObservedAt)
		if !protocol.ValidUUID(pending.CommandID) || parseError != nil || !address.Is4() ||
			timeError != nil || observedAt.IsZero() {
			return errors.New("IP state pending unchanged result is invalid")
		}
	}
	return nil
}

func (m *Monitor) Run(ctx context.Context, publishSnapshot func(protocol.IPSnapshotBody) error, publish func(protocol.IPObservationBody) error, publishUnchanged func(protocol.ChangeIPUnchangedBody) error) error {
	if publishSnapshot == nil || publish == nil || publishUnchanged == nil {
		return errors.New("IP observation publishers are required")
	}
	for {
		if err := m.step(ctx, publishSnapshot, publish, publishUnchanged); err != nil && ctx.Err() == nil {
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

func (m *Monitor) ArmChange(commandID, address string, startedAt time.Time) error {
	if !protocol.ValidUUID(commandID) {
		return errors.New("ChangeIP command ID is invalid")
	}
	parsed, err := netip.ParseAddr(address)
	if err != nil || !parsed.Is4() || startedAt.IsZero() {
		return errors.New("ChangeIP reconciliation input is invalid")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if (m.snapshot.LastIPv4 != "" && m.snapshot.LastIPv4 != address) ||
		m.snapshot.PendingSnapshot != nil || m.snapshot.Pending != nil || m.snapshot.PendingUnchanged != nil {
		return errors.New("IP monitor state does not match ChangeIP preflight")
	}
	if m.snapshot.ChangeAttempt != nil {
		if m.snapshot.ChangeAttempt.CommandID == commandID && m.snapshot.ChangeAttempt.Address == address {
			return nil
		}
		return errors.New("another ChangeIP reconciliation is active")
	}
	startedAt = startedAt.UTC()
	next := m.snapshot
	if next.LastIPv4 == "" {
		next.LastIPv4 = address
	}
	next.ChangeAttempt = &changeAttempt{
		CommandID: commandID, Address: address,
		ReconcileAt: startedAt.Add(changeReconcileGrace),
	}
	if err := m.file.Save(next); err != nil {
		return err
	}
	m.snapshot = next
	return nil
}

func (m *Monitor) CancelChange(commandID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.ChangeAttempt == nil || m.snapshot.ChangeAttempt.CommandID != commandID {
		return errors.New("ChangeIP reconciliation is not active")
	}
	next := m.snapshot
	next.ChangeAttempt = nil
	if err := m.file.Save(next); err != nil {
		return err
	}
	m.snapshot = next
	return nil
}

func (m *Monitor) HasChange(commandID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return (m.snapshot.ChangeAttempt != nil && m.snapshot.ChangeAttempt.CommandID == commandID) ||
		(m.snapshot.PendingUnchanged != nil && m.snapshot.PendingUnchanged.CommandID == commandID)
}

func (m *Monitor) ChangeAddress(commandID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.ChangeAttempt != nil && m.snapshot.ChangeAttempt.CommandID == commandID {
		return m.snapshot.ChangeAttempt.Address, true
	}
	if m.snapshot.PendingUnchanged != nil && m.snapshot.PendingUnchanged.CommandID == commandID {
		return m.snapshot.PendingUnchanged.Address, true
	}
	return "", false
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

func (m *Monitor) AckSnapshot(snapshotID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.PendingSnapshot == nil || m.snapshot.PendingSnapshot.SnapshotID != snapshotID {
		return errors.New("IP snapshot acknowledgment does not match pending state")
	}
	next := m.snapshot
	next.PendingSnapshot = nil
	if err := m.file.Save(next); err != nil {
		return err
	}
	m.snapshot = next
	return nil
}

func (m *Monitor) AckUnchanged(commandID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.PendingUnchanged == nil || m.snapshot.PendingUnchanged.CommandID != commandID {
		return errors.New("ChangeIP unchanged acknowledgment does not match pending state")
	}
	next := m.snapshot
	next.PendingUnchanged = nil
	if err := m.file.Save(next); err != nil {
		return err
	}
	m.snapshot = next
	return nil
}

func (m *Monitor) step(ctx context.Context, publishSnapshot func(protocol.IPSnapshotBody) error, publish func(protocol.IPObservationBody) error, publishUnchanged func(protocol.ChangeIPUnchangedBody) error) error {
	m.mu.Lock()
	if m.snapshot.PendingSnapshot != nil {
		pending := *m.snapshot.PendingSnapshot
		m.mu.Unlock()
		if err := publishSnapshot(pending); err != nil {
			return fmt.Errorf("%w: publish pending snapshot", errTransientMonitor)
		}
		return nil
	}
	if m.snapshot.Pending != nil {
		pending := *m.snapshot.Pending
		m.mu.Unlock()
		if err := publish(pending); err != nil {
			return fmt.Errorf("%w: publish pending observation", errTransientMonitor)
		}
		return nil
	}
	if m.snapshot.PendingUnchanged != nil {
		pending := *m.snapshot.PendingUnchanged
		m.mu.Unlock()
		if err := publishUnchanged(pending); err != nil {
			return fmt.Errorf("%w: publish pending unchanged result", errTransientMonitor)
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
	if m.snapshot.LastIPv4 == "" {
		pending := &protocol.IPSnapshotBody{
			SnapshotID: protocol.NewUUID(), Family: "ipv4", Address: current,
			ObservedAt: observation.ObservedAt.UTC().Format(time.RFC3339Nano),
		}
		next := m.snapshot
		next.LastIPv4 = current
		next.PendingSnapshot = pending
		if err := m.file.Save(next); err != nil {
			m.mu.Unlock()
			return err
		}
		m.snapshot = next
		m.mu.Unlock()
		if err := publishSnapshot(*pending); err != nil {
			return fmt.Errorf("%w: publish snapshot", errTransientMonitor)
		}
		return nil
	}
	if m.snapshot.LastIPv4 == current {
		if attempt := m.snapshot.ChangeAttempt; attempt != nil && !m.now().Before(attempt.ReconcileAt) {
			next := m.snapshot
			attemptCopy := *attempt
			attemptCopy.Confirmations++
			next.ChangeAttempt = &attemptCopy
			if next.ChangeAttempt.Confirmations >= changeUnchangedConfirmations {
				if err := m.persistUnchangedLocked(*next.ChangeAttempt, observation.ObservedAt); err != nil {
					m.mu.Unlock()
					return err
				}
				pending := *m.snapshot.PendingUnchanged
				m.mu.Unlock()
				if err := publishUnchanged(pending); err != nil {
					return fmt.Errorf("%w: publish unchanged result", errTransientMonitor)
				}
				return nil
			}
			if err := m.file.Save(next); err != nil {
				m.mu.Unlock()
				return err
			}
			m.snapshot = next
		}
		m.mu.Unlock()
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
	next.ChangeAttempt = nil
	if err := m.file.Save(next); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("persist IP observation: %w", err)
	}
	m.snapshot = next
	m.mu.Unlock()
	if err := publish(*pending); err != nil {
		return fmt.Errorf("%w: publish observation", errTransientMonitor)
	}
	return nil
}

func (m *Monitor) persistUnchangedLocked(attempt changeAttempt, observedAt time.Time) error {
	pending := &protocol.ChangeIPUnchangedBody{
		CommandID: attempt.CommandID, Address: attempt.Address,
		ObservedAt: observedAt.UTC().Format(time.RFC3339Nano),
	}
	next := m.snapshot
	next.ChangeAttempt = nil
	next.PendingUnchanged = pending
	if err := m.file.Save(next); err != nil {
		return fmt.Errorf("persist ChangeIP unchanged result: %w", err)
	}
	m.snapshot = next
	return nil
}
