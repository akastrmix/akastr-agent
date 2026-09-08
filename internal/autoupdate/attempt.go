package autoupdate

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/akastrmix/akastr-agent/internal/state"
)

// One record bounds retries across exec, service restart and host reboot.
type attemptRecord struct {
	Schema   int    `json:"schema"`
	Target   string `json:"target"`
	Attempts int    `json:"attempts"`
	RetryID  string `json:"retry_id"`
}

type attemptLedger struct {
	file   *state.JSONFile
	record attemptRecord
}

func loadAttempts(root, version string, revision int64, retryID string) (*attemptLedger, bool, error) {
	path := filepath.Join(root, "maintenance-attempt.json")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Size() > 4096 {
			return nil, false, errors.New("maintenance attempt record is unsafe")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	ledger := &attemptLedger{file: state.NewJSONFile(path)}
	found, err := ledger.file.Load(&ledger.record)
	if err != nil {
		return nil, false, err
	}
	if found && (ledger.record.Schema != 1 || !deploymentPattern.MatchString(ledger.record.Target) || ledger.record.Attempts < 0 || ledger.record.Attempts > 2 || len(ledger.record.RetryID) > 64) {
		return nil, false, errors.New("maintenance attempt record is invalid")
	}
	target := deploymentName(version, revision)
	if !found || ledger.record.Target != target {
		pending, err := uncommittedDeploymentExists(root, version, revision)
		if err != nil {
			return nil, false, err
		}
		ledger.record = attemptRecord{Schema: 1, Target: target}
		// An interrupted deployment from an older updater already used its first try.
		if pending {
			ledger.record.Attempts = 1
		}
	}
	manual := retryID != "" && retryID != ledger.record.RetryID
	if len(retryID) > 64 {
		return nil, false, errors.New("maintenance retry ID is invalid")
	}
	if manual {
		ledger.record.RetryID = retryID
		if ledger.record.Attempts >= 2 {
			ledger.record.Attempts = 1
		}
		if err := ledger.file.Save(ledger.record); err != nil {
			return nil, false, err
		}
	}
	return ledger, manual, nil
}

func (ledger *attemptLedger) begin() error {
	if ledger.record.Attempts >= 2 {
		return errors.New("maintenance trial attempts exhausted")
	}
	ledger.record.Attempts++
	// Persist before preparing/replacing the process, including the crash window.
	return ledger.file.Save(ledger.record)
}
