package xui

import (
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"

	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/state"
)

// localState is the module's only durable state.
//
// resets holds, per owned client, the last reset_seq whose traffic reset this
// node executed. Memory is the truth: a reset that ran is recorded at once,
// and while saving it fails the record stays dirty and is saved before any
// further work, so a failed save never runs the same reset again. Only a stop
// before the save repeats it, losing at most the traffic of that moment.
//
// restartPending is saved before a Shadowsocks 2022 inbound is written and
// cleared after Xray restarted, so a failed restart or a stop in between is
// restarted later. It changes in memory only once saved.
type localState struct {
	file           *state.JSONFile
	resets         map[string]int64
	restartPending bool
	dirty          bool
}

type stateFile struct {
	Schema         int              `json:"schema"`
	Resets         map[string]int64 `json:"resets"`
	RestartPending bool             `json:"restart_pending"`
}

// openState starts empty only when the file does not exist; an incomplete
// file would silently drop reset records, so it stops the Agent instead.
func openState(stateDir string) (*localState, error) {
	file := state.NewJSONFile(filepath.Join(stateDir, "state.json"))
	var raw json.RawMessage
	found, err := file.Load(&raw)
	if err != nil {
		return nil, err
	}
	local := &localState{file: file, resets: map[string]int64{}}
	if !found {
		return local, nil
	}
	stored, err := protocol.DecodeStrict[stateFile](raw, "xui state", "schema", "resets", "restart_pending")
	if err != nil || stored.Schema != 1 {
		return nil, errors.New("xui state file is incomplete or has an unknown schema")
	}
	local.resets, local.restartPending = stored.Resets, stored.RestartPending
	return local, nil
}

func (s *localState) save(restartPending bool) error {
	return s.file.Save(stateFile{Schema: 1, Resets: s.resets, RestartPending: restartPending})
}

func (s *localState) reset(email string) int64 { return s.resets[email] }

// setReset records an executed reset and tries to save it.
func (s *localState) setReset(email string, seq int64) error {
	s.resets[email] = seq
	s.dirty = true
	return s.flush()
}

// flush saves records that a failed save left in memory.
func (s *localState) flush() error {
	if !s.dirty {
		return nil
	}
	if err := s.save(s.restartPending); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

func (s *localState) setRestartPending(pending bool) error {
	if s.restartPending == pending {
		return nil
	}
	if err := s.save(pending); err != nil {
		return err
	}
	s.restartPending, s.dirty = pending, false
	return nil
}

// keepOnly forgets clients Cloud no longer has.
func (s *localState) keepOnly(emails map[string]bool) error {
	kept := maps.Clone(s.resets)
	maps.DeleteFunc(kept, func(email string, _ int64) bool { return !emails[email] })
	if len(kept) == len(s.resets) {
		return nil
	}
	s.resets, s.dirty = kept, true
	return s.flush()
}
