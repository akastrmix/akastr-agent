package xui

import (
	"errors"
	"maps"
	"path/filepath"

	"github.com/akastrmix/akastr-agent/internal/state"
)

// localState is the module's only durable state. Resets holds, per owned
// client, the last reset_seq whose traffic reset this node executed, so a
// target sent again never clears traffic twice. RestartPending is set before
// a Shadowsocks 2022 inbound is written and cleared after Xray restarted, so
// a failed restart or a stop in between is restarted later.
type localState struct {
	file   *state.JSONFile
	stored stateFile
}

type stateFile struct {
	Schema         int              `json:"schema"`
	Resets         map[string]int64 `json:"resets"`
	RestartPending bool             `json:"restart_pending"`
}

func openState(stateDir string) (*localState, error) {
	file := state.NewJSONFile(filepath.Join(stateDir, "state.json"))
	stored := stateFile{Schema: 1, Resets: map[string]int64{}}
	if _, err := file.Load(&stored); err != nil {
		return nil, err
	}
	if stored.Schema != 1 || stored.Resets == nil {
		return nil, errors.New("xui state has an unknown schema")
	}
	return &localState{file: file, stored: stored}, nil
}

func (s *localState) reset(email string) int64 { return s.stored.Resets[email] }

// update saves a changed copy and adopts it only once it is on disk, so memory
// never claims a reset or restart the disk does not.
func (s *localState) update(change func(*stateFile)) error {
	next := s.stored
	next.Resets = maps.Clone(s.stored.Resets)
	change(&next)
	if err := s.file.Save(next); err != nil {
		return err
	}
	s.stored = next
	return nil
}

func (s *localState) setReset(email string, seq int64) error {
	return s.update(func(f *stateFile) { f.Resets[email] = seq })
}

func (s *localState) setRestartPending(pending bool) error {
	if s.stored.RestartPending == pending {
		return nil
	}
	return s.update(func(f *stateFile) { f.RestartPending = pending })
}

// keepOnly forgets clients Cloud no longer has.
func (s *localState) keepOnly(emails map[string]bool) error {
	stale := false
	for email := range s.stored.Resets {
		stale = stale || !emails[email]
	}
	if !stale {
		return nil
	}
	return s.update(func(f *stateFile) {
		for email := range f.Resets {
			if !emails[email] {
				delete(f.Resets, email)
			}
		}
	})
}
