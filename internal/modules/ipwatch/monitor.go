package ipwatch

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/state"
)

type AddressObserver interface {
	Observe(context.Context, Family) (Observation, error)
}

// Cloud holds the address history; the node only says what its address is now.
// The one durable fact is a ChangeIP reconciliation, kept until Cloud has
// acknowledged how it ended.
type monitorState struct {
	SchemaVersion int            `json:"schema_version"`
	ChangeAttempt *changeAttempt `json:"change_attempt,omitempty"`
}

type changeAttempt struct {
	CommandID     string    `json:"command_id"`
	Address       string    `json:"address"`
	ReconcileAt   time.Time `json:"reconcile_at"`
	Confirmations int       `json:"confirmations"`
	Outcome       *outcome  `json:"outcome,omitempty"`
}

// outcome is how an attempt ended: the address changed, or stayed the same.
type outcome struct {
	ReportID   string `json:"report_id"`
	Changed    bool   `json:"changed"`
	Address    string `json:"address"`
	ObservedAt string `json:"observed_at"`
}

// familyState is what this process told Cloud about one address family.
type familyState struct {
	reported string         // the address Cloud acknowledged
	pending  *addressReport // sent and not yet acknowledged
	announce bool           // a new session is owed the current address
}

type addressReport struct {
	id   string
	data AddressData
}

type Monitor struct {
	mu          sync.Mutex
	file        *state.JSONFile
	observer    AddressObserver
	interval    time.Duration
	now         func() time.Time
	wake        chan struct{}
	wakeIPv6    chan struct{}
	observeIPv6 bool
	persisted   monitorState
	ipv4        familyState
	ipv6        familyState
}

var errTransientMonitor = errors.New("transient IP monitor failure")

// A ChangeIP normally drops the network and the address seen once it is back
// is final, so after a short grace two observations of the old address settle
// the attempt. While an attempt is open the address is observed every
// changeObserveInterval instead of the configured interval; failed
// observations never count.
const (
	changeReconcileGrace         = 2 * time.Minute
	changeUnchangedConfirmations = 2
	changeObserveInterval        = 10 * time.Second
)

// ReconciliationFile is where an open ChangeIP reconciliation is kept.
func ReconciliationFile(stateDir string) string {
	return filepath.Join(stateDir, "changeip-reconciliation.json")
}

func OpenMonitor(filePath string, observer AddressObserver, interval time.Duration, observeIPv6 bool) (*Monitor, error) {
	if observer == nil || interval < 10*time.Second || interval > 5*time.Minute {
		return nil, errors.New("IP monitor options are invalid")
	}
	monitor := &Monitor{
		file: state.NewJSONFile(filePath), observer: observer, interval: interval, now: time.Now,
		wake: make(chan struct{}, 1), wakeIPv6: make(chan struct{}, 1), observeIPv6: observeIPv6,
		persisted: monitorState{SchemaVersion: 1},
		// The first observation of every process is reported.
		ipv4: familyState{announce: true}, ipv6: familyState{announce: true},
	}
	found, err := monitor.file.Load(&monitor.persisted)
	if err != nil {
		return nil, err
	}
	if found {
		if err := validateMonitorState(monitor.persisted); err != nil {
			return nil, err
		}
	}
	return monitor, nil
}

func validateMonitorState(persisted monitorState) error {
	if persisted.SchemaVersion != 1 {
		return errors.New("ChangeIP reconciliation schema is unsupported")
	}
	attempt := persisted.ChangeAttempt
	if attempt == nil {
		return nil
	}
	address, parseError := netip.ParseAddr(attempt.Address)
	if !protocol.ValidUUID(attempt.CommandID) || parseError != nil || !address.Is4() ||
		attempt.ReconcileAt.IsZero() || attempt.Confirmations < 0 {
		return errors.New("ChangeIP reconciliation is invalid")
	}
	if result := attempt.Outcome; result != nil {
		address, addressError := netip.ParseAddr(result.Address)
		observedAt, timeError := time.Parse(time.RFC3339Nano, result.ObservedAt)
		if !protocol.ValidUUID(result.ReportID) || addressError != nil || !address.Is4() ||
			result.Changed == (result.Address == attempt.Address) || timeError != nil || observedAt.IsZero() {
			return errors.New("ChangeIP reconciliation outcome is invalid")
		}
	}
	return nil
}

func (m *Monitor) Run(ctx context.Context, publish func(protocol.ReportBody) error) error {
	if publish == nil {
		return errors.New("IP report publisher is required")
	}
	if !m.observeIPv6 {
		return m.runIPv4(ctx, publish)
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- m.runIPv4(runContext, publish) }()
	go func() { done <- m.runIPv6(runContext, publish) }()
	err := <-done
	cancel()
	<-done
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (m *Monitor) runIPv4(ctx context.Context, publish func(protocol.ReportBody) error) error {
	return m.runLoop(ctx, m.wake, m.ipv4Delay, func() error { return m.stepIPv4(ctx, publish) })
}

func (m *Monitor) ipv4Delay() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.persisted.ChangeAttempt != nil && m.interval > changeObserveInterval {
		return changeObserveInterval
	}
	return m.interval
}

func (m *Monitor) runIPv6(ctx context.Context, publish func(protocol.ReportBody) error) error {
	return m.runLoop(ctx, m.wakeIPv6, func() time.Duration { return m.interval }, func() error {
		return m.stepIPv6(ctx, publish)
	})
}

func (m *Monitor) runLoop(ctx context.Context, wake <-chan struct{}, delay func() time.Duration, step func() error) error {
	for {
		if err := step(); err != nil && ctx.Err() == nil && !errors.Is(err, errTransientMonitor) {
			return err
		}
		timer := time.NewTimer(delay())
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// NotifyControlReady owes each new session the current address: a report that
// was waiting is replaced by a fresh observation.
func (m *Monitor) NotifyControlReady() {
	m.mu.Lock()
	m.ipv4.pending, m.ipv4.announce = nil, true
	m.ipv6.pending, m.ipv6.announce = nil, true
	m.mu.Unlock()
	wakeMonitor(m.wake)
	if m.observeIPv6 {
		wakeMonitor(m.wakeIPv6)
	}
}

func wakeMonitor(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// AddressSettled reports whether Cloud holds the IPv4 address this node has
// now, so a ChangeIP starts from the address both sides know.
func (m *Monitor) AddressSettled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ipv4.reported != "" && m.ipv4.pending == nil && !m.ipv4.announce
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
	if attempt := m.persisted.ChangeAttempt; attempt != nil {
		if attempt.CommandID == commandID && attempt.Address == address {
			return nil
		}
		return errors.New("another ChangeIP reconciliation is active")
	}
	if m.ipv4.reported != address || m.ipv4.pending != nil || m.ipv4.announce {
		return errors.New("IP monitor state does not match ChangeIP preflight")
	}
	next := m.persisted
	next.ChangeAttempt = &changeAttempt{
		CommandID: commandID, Address: address, ReconcileAt: startedAt.UTC().Add(changeReconcileGrace),
	}
	if err := m.file.Save(next); err != nil {
		return err
	}
	m.persisted = next
	wakeMonitor(m.wake) // Switch to the fast cadence now rather than after the current wait.
	return nil
}

func (m *Monitor) CancelChange(commandID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	attempt := m.persisted.ChangeAttempt
	if attempt == nil || attempt.CommandID != commandID || attempt.Outcome != nil {
		return errors.New("ChangeIP reconciliation is not active")
	}
	next := m.persisted
	next.ChangeAttempt = nil
	if err := m.file.Save(next); err != nil {
		return err
	}
	m.persisted = next
	return nil
}

func (m *Monitor) ChangeAddress(commandID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if attempt := m.persisted.ChangeAttempt; attempt != nil && attempt.CommandID == commandID {
		return attempt.Address, true
	}
	return "", false
}

// Acknowledge settles the report Cloud stored; false means this module did not send it.
func (m *Monitor) Acknowledge(reportID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if attempt := m.persisted.ChangeAttempt; attempt != nil && attempt.Outcome != nil && attempt.Outcome.ReportID == reportID {
		next := m.persisted
		next.ChangeAttempt = nil
		if err := m.file.Save(next); err != nil {
			return true, err
		}
		m.persisted = next
		m.ipv4.reported = attempt.Outcome.Address
		wakeMonitor(m.wake)
		return true, nil
	}
	for _, family := range []struct {
		state *familyState
		wake  chan struct{}
	}{{&m.ipv4, m.wake}, {&m.ipv6, m.wakeIPv6}} {
		if pending := family.state.pending; pending != nil && pending.id == reportID {
			family.state.reported, family.state.pending = pending.data.Address, nil
			wakeMonitor(family.wake)
			return true, nil
		}
	}
	return false, nil
}

func (m *Monitor) stepIPv4(ctx context.Context, publish func(protocol.ReportBody) error) error {
	m.mu.Lock()
	if attempt := m.persisted.ChangeAttempt; attempt != nil && attempt.Outcome != nil {
		report := outcomeReport(*attempt)
		m.mu.Unlock()
		return publishReport(publish, report)
	}
	if pending := m.ipv4.pending; pending != nil {
		m.mu.Unlock()
		return publishReport(publish, pending.body())
	}
	m.mu.Unlock()

	observation, err := m.observer.Observe(ctx, IPv4)
	if err != nil {
		return fmt.Errorf("%w: observe IPv4", errTransientMonitor)
	}
	current := observation.Address.String()
	observedAt := observation.ObservedAt.UTC().Format(time.RFC3339Nano)
	m.mu.Lock()
	if attempt := m.persisted.ChangeAttempt; attempt != nil {
		// Only this attempt's outcome may report the address until it settles, so
		// a change it caused is never reported as a natural one.
		next := m.persisted
		settled := *attempt
		if current != attempt.Address {
			settled.Outcome = &outcome{ReportID: protocol.NewUUID(), Changed: true, Address: current, ObservedAt: observedAt}
		} else if !m.now().Before(attempt.ReconcileAt) {
			settled.Confirmations++
			if settled.Confirmations >= changeUnchangedConfirmations {
				settled.Outcome = &outcome{ReportID: protocol.NewUUID(), Address: current, ObservedAt: observedAt}
			}
		} else {
			m.mu.Unlock()
			return nil
		}
		next.ChangeAttempt = &settled
		if err := m.file.Save(next); err != nil {
			m.mu.Unlock()
			return fmt.Errorf("persist ChangeIP reconciliation: %w", err)
		}
		m.persisted = next
		m.mu.Unlock()
		if settled.Outcome == nil {
			return nil
		}
		return publishReport(publish, outcomeReport(settled))
	}
	report := m.ipv4.observed(current, observedAt, "ipv4")
	m.mu.Unlock()
	if report == nil {
		return nil
	}
	return publishReport(publish, *report)
}

func (m *Monitor) stepIPv6(ctx context.Context, publish func(protocol.ReportBody) error) error {
	m.mu.Lock()
	if pending := m.ipv6.pending; pending != nil {
		m.mu.Unlock()
		return publishReport(publish, pending.body())
	}
	m.mu.Unlock()
	observation, err := m.observer.Observe(ctx, IPv6)
	if err != nil {
		return fmt.Errorf("%w: observe IPv6", errTransientMonitor)
	}
	m.mu.Lock()
	report := m.ipv6.observed(observation.Address.String(), observation.ObservedAt.UTC().Format(time.RFC3339Nano), "ipv6")
	m.mu.Unlock()
	if report == nil {
		return nil
	}
	return publishReport(publish, *report)
}

// observed turns an observation into the report Cloud is owed, if any. The
// caller holds the monitor lock.
func (f *familyState) observed(address, observedAt, family string) *protocol.ReportBody {
	if !f.announce && address == f.reported {
		return nil
	}
	f.announce = false
	f.pending = &addressReport{
		id:   protocol.NewUUID(),
		data: AddressData{Family: family, Address: address, ObservedAt: observedAt},
	}
	report := f.pending.body()
	return &report
}

func (r addressReport) body() protocol.ReportBody {
	return protocol.ReportBody{ReportID: r.id, Kind: KindAddress, Data: r.data}
}

func outcomeReport(attempt changeAttempt) protocol.ReportBody {
	result := attempt.Outcome
	if result.Changed {
		commandID := attempt.CommandID
		return protocol.ReportBody{ReportID: result.ReportID, Kind: KindAddress, Data: AddressData{
			Family: "ipv4", Address: result.Address, ObservedAt: result.ObservedAt, CommandID: &commandID,
		}}
	}
	return protocol.ReportBody{ReportID: result.ReportID, Kind: KindUnchanged, Data: UnchangedData{
		CommandID: attempt.CommandID, Address: result.Address, ObservedAt: result.ObservedAt,
	}}
}

func publishReport(publish func(protocol.ReportBody) error, report protocol.ReportBody) error {
	if err := publish(report); err != nil {
		return fmt.Errorf("%w: publish %s", errTransientMonitor, report.Kind)
	}
	return nil
}

func (m *Monitor) reconciling() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.persisted.ChangeAttempt != nil
}
