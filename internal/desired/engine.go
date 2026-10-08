// Package desired keeps the Cloud-owned targets of Desired modules and applies
// them: shortly after a change, every few minutes to undo local drift, and with
// backoff after a failure. Targets live only in memory because Cloud sends
// every key again, followed by the key list, whenever a session becomes ready;
// until the first key list arrives a module is never applied, so a partial set
// cannot remove what a missing key still owns.
package desired

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const (
	settleDelay  = 2 * time.Second
	recheckDelay = 5 * time.Minute
	firstRetry   = 30 * time.Second
	applyTimeout = 2 * time.Minute
	// invalidCode answers a target its module rejected; the key keeps its
	// previous target, so retrying cannot help and Cloud must send another.
	invalidCode = "target_invalid"
)

// Send delivers one status on the current control session.
type Send func(protocol.StateStatus) error

type Engine struct {
	units map[string]*unit
}

type target struct {
	version int64
	state   json.RawMessage
}

type status struct {
	version int64
	code    string
}

type unit struct {
	name   string
	module module.Desired
	wake   chan struct{}
	// Timing is fixed in production; tests shorten it.
	settle, recheck, retry time.Duration

	mu       sync.Mutex
	targets  map[string]target
	invalid  map[string]int64
	complete bool
	sent     map[string]status
}

func New() *Engine {
	return &Engine{units: map[string]*unit{}}
}

// Add registers the Desired module configured under name.
func (e *Engine) Add(name string, m module.Desired) {
	e.units[name] = &unit{
		name: name, module: m, wake: make(chan struct{}, 1),
		settle: settleDelay, recheck: recheckDelay, retry: firstRetry,
		targets: map[string]target{}, invalid: map[string]int64{}, sent: map[string]status{},
	}
}

func (e *Engine) unit(name string) (*unit, error) {
	u, found := e.units[name]
	if !found {
		return nil, fmt.Errorf("state for module %q, which is not enabled on this node", name)
	}
	return u, nil
}

// Put replaces the target of one key. A target the module rejects leaves the
// previous one in place, or the key untouched when it has none, and is
// reported as invalid.
func (e *Engine) Put(put protocol.StatePut) error {
	u, err := e.unit(put.Module)
	if err != nil {
		return err
	}
	valid := u.module.Validate(put.Key, put.State) == nil
	u.mu.Lock()
	if valid {
		u.targets[put.Key] = target{version: put.Version, state: put.State}
		delete(u.invalid, put.Key)
	} else {
		u.invalid[put.Key] = put.Version
	}
	u.mu.Unlock()
	u.signal()
	return nil
}

// Keys retires every key Cloud no longer names and lets the module apply.
func (e *Engine) Keys(keys protocol.StateKeys) error {
	u, err := e.unit(keys.Module)
	if err != nil {
		return err
	}
	named := make(map[string]bool, len(keys.Keys))
	for _, key := range keys.Keys {
		named[key] = true
	}
	u.mu.Lock()
	for key := range u.targets {
		if !named[key] {
			delete(u.targets, key)
		}
	}
	for key := range u.invalid {
		if !named[key] {
			delete(u.invalid, key)
		}
	}
	u.complete = true
	u.mu.Unlock()
	u.signal()
	return nil
}

// ControlReady forgets what earlier sessions were told, so the next pass
// reports every key again.
func (e *Engine) ControlReady() {
	for _, u := range e.units {
		u.mu.Lock()
		u.sent = map[string]status{}
		u.mu.Unlock()
	}
}

// Run applies every module until ctx ends.
func (e *Engine) Run(ctx context.Context, send Send) {
	var wg sync.WaitGroup
	for _, u := range e.units {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u.run(ctx, send)
		}()
	}
	wg.Wait()
}

func (u *unit) signal() {
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

func (u *unit) run(ctx context.Context, send Send) {
	timer := time.NewTimer(u.recheck)
	defer timer.Stop()
	var backoff time.Duration
	for {
		select {
		case <-ctx.Done():
			return
		case <-u.wake:
			// Cloud sends a burst of keys at once; apply them together.
			select {
			case <-ctx.Done():
				return
			case <-time.After(u.settle):
			}
			select {
			case <-u.wake:
			default:
			}
		case <-timer.C:
		}
		next := u.recheck
		if u.pass(ctx, send) {
			backoff = min(max(u.retry, 2*backoff), u.recheck)
			next = backoff
		} else {
			backoff = 0
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(next)
	}
}

// pass applies the current targets once and reports what changed. It returns
// true when a key failed in a way a later attempt may fix.
func (u *unit) pass(ctx context.Context, send Send) bool {
	u.mu.Lock()
	if !u.complete {
		u.mu.Unlock()
		return false
	}
	states := make(map[string]json.RawMessage, len(u.targets))
	results := make(map[string]status, len(u.targets)+len(u.invalid))
	for key, t := range u.targets {
		states[key] = t.state
		results[key] = status{version: t.version}
	}
	invalid := make(map[string]int64, len(u.invalid))
	for key, version := range u.invalid {
		invalid[key] = version
		if _, found := states[key]; !found {
			// Still named, so what the key covers must not be removed.
			states[key] = nil
		}
	}
	u.mu.Unlock()

	applyContext, cancel := context.WithTimeout(ctx, applyTimeout)
	codes, err := u.module.Apply(applyContext, states)
	cancel()
	failed := err != nil
	if err != nil {
		slog.Warn("target state not fully applied", "module", u.name, "code", err.Error())
	}
	for key, result := range results {
		if code := codes[key]; code != "" {
			result.code = code
			results[key] = result
			failed = true
		}
	}
	for key, version := range invalid {
		results[key] = status{version: version, code: invalidCode}
	}

	for key, result := range results {
		u.mu.Lock()
		unchanged := u.sent[key] == result
		u.mu.Unlock()
		if unchanged {
			continue
		}
		if send(protocol.StateStatus{Module: u.name, Key: key, Version: result.version, ErrorCode: result.code}) != nil {
			continue
		}
		u.mu.Lock()
		u.sent[key] = result
		u.mu.Unlock()
	}
	return failed
}
