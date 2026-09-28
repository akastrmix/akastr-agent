package update

import (
	"errors"
	"time"

	"github.com/akastrmix/akastr-agent/internal/layout"
	"github.com/akastrmix/akastr-agent/internal/state"
)

// A failed candidate is restarted as the previous deployment by systemd. The
// record, written before the process is replaced, bounds how often one target is
// tried: twice in a row, then once per retryExhaustedAfter, so a transient fault
// recovers on its own while a broken target cannot cause a restart loop.
const (
	maxConsecutiveAttempts = 2
	retryExhaustedAfter    = 6 * time.Hour
)

type attemptRecord struct {
	Schema        int       `json:"schema"`
	Target        string    `json:"target"`
	Attempts      int       `json:"attempts"`
	LastAttemptAt time.Time `json:"last_attempt_at"`
	LastError     string    `json:"last_error,omitempty"`
}

func loadAttempts(path string) (attemptRecord, error) {
	var record attemptRecord
	found, err := state.NewJSONFile(path).Load(&record)
	if err != nil {
		return attemptRecord{}, err
	}
	if !found {
		return attemptRecord{Schema: 1}, nil
	}
	if record.Schema != 1 || record.Attempts < 0 || (record.LastError != "" && !stableCode.MatchString(record.LastError)) {
		return attemptRecord{}, errors.New("update attempt record is invalid")
	}
	return record, nil
}

func (r attemptRecord) allowed(now time.Time) bool {
	return r.Attempts < maxConsecutiveAttempts || now.Sub(r.LastAttemptAt) >= retryExhaustedAfter
}

func (r attemptRecord) failureCode() string {
	if r.LastError != "" {
		return r.LastError
	}
	return "candidate_failed"
}

// RecordCandidateFailure stores why a candidate stopped. The deployment that
// systemd restarts reports it once the target's attempts are exhausted.
func RecordCandidateFailure(paths layout.Layout, target, code string) error {
	path := paths.AttemptFile()
	record, err := loadAttempts(path)
	if err != nil {
		return err
	}
	if record.Target != target || !stableCode.MatchString(code) {
		return errors.New("candidate failure does not match the recorded attempt")
	}
	record.LastError = code
	return state.NewJSONFile(path).Save(record)
}

func TargetName(version string, revision int64) string {
	return Target{Version: version, ConfigurationRevision: revision}.Name()
}
