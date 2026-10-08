package desired

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type recordingModule struct {
	mu      sync.Mutex
	applied []map[string]string
	fail    map[string]string
	err     error
}

func (m *recordingModule) Validate(_ string, state json.RawMessage) error {
	if string(state) == `{"bad":true}` {
		return errors.New("bad")
	}
	return nil
}

func (m *recordingModule) Apply(_ context.Context, targets map[string]json.RawMessage) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]string{}
	for key, state := range targets {
		seen[key] = string(state)
	}
	m.applied = append(m.applied, seen)
	return m.fail, m.err
}

func (m *recordingModule) passes() []map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]string(nil), m.applied...)
}

type statusLog struct {
	mu   sync.Mutex
	sent []protocol.StateStatus
}

func (s *statusLog) send(status protocol.StateStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, status)
	return nil
}

func (s *statusLog) take() []protocol.StateStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	sent := s.sent
	s.sent = nil
	return sent
}

func startEngine(t *testing.T, m *recordingModule) (*Engine, *statusLog) {
	t.Helper()
	engine := New()
	engine.Add("xui", m)
	u := engine.units["xui"]
	u.settle, u.recheck, u.retry = 10*time.Millisecond, time.Hour, 20*time.Millisecond
	log := &statusLog{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		engine.Run(ctx, log.send)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return engine, log
}

func put(key string, version int64, state string) protocol.StatePut {
	return protocol.StatePut{Module: "xui", Key: key, Version: version, State: json.RawMessage(state)}
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNothingIsAppliedBeforeTheKeyListAndRetiredKeysDrop(t *testing.T) {
	m := &recordingModule{}
	engine, log := startEngine(t, m)
	_ = engine.Put(put("1", 1, `{"a":1}`))
	_ = engine.Put(put("2", 1, `{"b":1}`))
	time.Sleep(50 * time.Millisecond)
	if len(m.passes()) != 0 {
		t.Fatal("applied a possibly partial target set")
	}
	_ = engine.Keys(protocol.StateKeys{Module: "xui", Keys: []string{"1", "2"}})
	eventually(t, func() bool { return len(m.passes()) == 1 })
	if pass := m.passes()[0]; len(pass) != 2 {
		t.Fatalf("pass %v", pass)
	}
	eventually(t, func() bool { return len(log.take()) == 2 })
	_ = engine.Keys(protocol.StateKeys{Module: "xui", Keys: []string{"2"}})
	eventually(t, func() bool { return len(m.passes()) == 2 })
	if pass := m.passes()[1]; len(pass) != 1 || pass["2"] == "" {
		t.Fatalf("retired key still applied: %v", pass)
	}
}

func TestStatusIsSentOncePerVersionAndAgainForANewSession(t *testing.T) {
	m := &recordingModule{}
	engine, log := startEngine(t, m)
	_ = engine.Put(put("1", 4, `{"a":1}`))
	_ = engine.Keys(protocol.StateKeys{Module: "xui", Keys: []string{"1"}})
	eventually(t, func() bool { return len(m.passes()) == 1 })
	eventually(t, func() bool {
		sent := log.take()
		return len(sent) == 1 && sent[0] == protocol.StateStatus{Module: "xui", Key: "1", Version: 4}
	})
	_ = engine.Keys(protocol.StateKeys{Module: "xui", Keys: []string{"1"}})
	eventually(t, func() bool { return len(m.passes()) == 2 })
	if sent := log.take(); len(sent) != 0 {
		t.Fatalf("unchanged status resent: %v", sent)
	}
	engine.ControlReady()
	_ = engine.Keys(protocol.StateKeys{Module: "xui", Keys: []string{"1"}})
	eventually(t, func() bool { return len(log.take()) == 1 })
}

func TestInvalidTargetKeepsThePreviousOne(t *testing.T) {
	m := &recordingModule{}
	engine, log := startEngine(t, m)
	_ = engine.Put(put("1", 1, `{"a":1}`))
	_ = engine.Put(put("1", 2, `{"bad":true}`))
	_ = engine.Keys(protocol.StateKeys{Module: "xui", Keys: []string{"1"}})
	eventually(t, func() bool { return len(m.passes()) == 1 })
	if m.passes()[0]["1"] != `{"a":1}` {
		t.Fatal("invalid target replaced the previous one")
	}
	eventually(t, func() bool {
		sent := log.take()
		return len(sent) == 1 && sent[0].Version == 2 && sent[0].ErrorCode == invalidCode
	})
}

func TestInvalidFirstTargetLeavesTheKeyUntouched(t *testing.T) {
	m := &recordingModule{}
	engine, _ := startEngine(t, m)
	_ = engine.Put(put("1", 1, `{"bad":true}`))
	_ = engine.Keys(protocol.StateKeys{Module: "xui", Keys: []string{"1"}})
	eventually(t, func() bool { return len(m.passes()) == 1 })
	if state, named := m.passes()[0]["1"]; !named || state != "" {
		t.Fatalf("a key named without a usable target must reach the module as nil: %q %v", state, named)
	}
}

func TestFailuresAreRetried(t *testing.T) {
	for name, m := range map[string]*recordingModule{
		"key":     {fail: map[string]string{"1": "xui_login_failed"}},
		"cleanup": {err: errors.New("xui_cleanup_failed")},
	} {
		t.Run(name, func(t *testing.T) {
			engine, _ := startEngine(t, m)
			_ = engine.Put(put("1", 1, `{"a":1}`))
			_ = engine.Keys(protocol.StateKeys{Module: "xui", Keys: []string{"1"}})
			eventually(t, func() bool { return len(m.passes()) >= 3 })
		})
	}
}

func TestStateForAModuleNotEnabledIsRejected(t *testing.T) {
	engine := New()
	if engine.Put(put("1", 1, `{}`)) == nil {
		t.Fatal("accepted state for a module this node does not run")
	}
}
