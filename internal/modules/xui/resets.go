package xui

import (
	"errors"
	"path/filepath"

	"github.com/akastrmix/akastr-agent/internal/state"
)

// resetMarks remembers, per owned client, the last reset_seq whose traffic
// reset this node executed, so a target sent again never clears traffic twice.
type resetMarks struct {
	file  *state.JSONFile
	marks map[string]int64
}

type resetFile struct {
	Schema int              `json:"schema"`
	Marks  map[string]int64 `json:"marks"`
}

func openResetMarks(stateDir string) (*resetMarks, error) {
	file := state.NewJSONFile(filepath.Join(stateDir, "resets.json"))
	var stored resetFile
	found, err := file.Load(&stored)
	if err != nil {
		return nil, err
	}
	if !found {
		stored = resetFile{Schema: 1, Marks: map[string]int64{}}
	}
	if stored.Schema != 1 || stored.Marks == nil {
		return nil, errors.New("xui reset state has an unknown schema")
	}
	return &resetMarks{file: file, marks: stored.Marks}, nil
}

func (r *resetMarks) get(email string) int64 { return r.marks[email] }

func (r *resetMarks) set(email string, seq int64) error {
	r.marks[email] = seq
	return r.save()
}

// keepOnly forgets clients Cloud no longer has.
func (r *resetMarks) keepOnly(emails map[string]bool) error {
	changed := false
	for email := range r.marks {
		if !emails[email] {
			delete(r.marks, email)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.save()
}

func (r *resetMarks) save() error {
	return r.file.Save(resetFile{Schema: 1, Marks: r.marks})
}
