package operation

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/state"
)

const StateSchemaVersion = 1

var (
	ErrConflict  = errors.New("operation exclusive group is busy")
	ErrDuplicate = errors.New("operation id already exists")
	ErrNotFound  = errors.New("active operation not found")
	stableToken  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
)

type Status string

const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

type Record struct {
	OperationID    string     `json:"operation_id"`
	Kind           string     `json:"kind"`
	ExclusiveGroup string     `json:"exclusive_group"`
	Status         Status     `json:"status"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	TerminalCode   string     `json:"terminal_code,omitempty"`
}

type Snapshot struct {
	SchemaVersion int               `json:"schema_version"`
	Active        map[string]Record `json:"active"`
	Recent        []Record          `json:"recent"`
}

type Engine struct {
	mu          sync.Mutex
	file        *state.JSONFile
	now         func() time.Time
	recentLimit int
	snapshot    Snapshot
}

type Options struct {
	StateFile   string
	RecentLimit int
	Now         func() time.Time
}

func Open(options Options) (*Engine, error) {
	if options.StateFile == "" {
		return nil, errors.New("operation state file is required")
	}
	if options.RecentLimit < 1 {
		return nil, errors.New("operation recent limit must be positive")
	}
	if options.Now == nil {
		options.Now = time.Now
	}

	engine := &Engine{
		file:        state.NewJSONFile(options.StateFile),
		now:         options.Now,
		recentLimit: options.RecentLimit,
		snapshot: Snapshot{
			SchemaVersion: StateSchemaVersion,
			Active:        map[string]Record{},
			Recent:        []Record{},
		},
	}

	found, err := engine.file.Load(&engine.snapshot)
	if err != nil {
		return nil, err
	}
	if found {
		if err := validateSnapshot(engine.snapshot); err != nil {
			return nil, fmt.Errorf("validate operation state: %w", err)
		}
	}
	return engine, nil
}

func (e *Engine) Begin(operationID, kind, exclusiveGroup string) (Record, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !stableToken.MatchString(operationID) {
		return Record{}, errors.New("operation id must be a stable lowercase token")
	}
	if !stableToken.MatchString(kind) {
		return Record{}, errors.New("operation kind must be a stable lowercase token")
	}
	if !stableToken.MatchString(exclusiveGroup) {
		return Record{}, errors.New("exclusive group must be a stable lowercase token")
	}
	if _, exists := e.snapshot.Active[exclusiveGroup]; exists {
		return Record{}, fmt.Errorf("%w: %s", ErrConflict, exclusiveGroup)
	}
	if containsOperationID(e.snapshot, operationID) {
		return Record{}, fmt.Errorf("%w: %s", ErrDuplicate, operationID)
	}

	record := Record{
		OperationID:    operationID,
		Kind:           kind,
		ExclusiveGroup: exclusiveGroup,
		Status:         StatusRunning,
		StartedAt:      e.now().UTC(),
	}
	next := cloneSnapshot(e.snapshot)
	next.Active[exclusiveGroup] = record
	if err := e.file.Save(next); err != nil {
		return Record{}, err
	}
	e.snapshot = next
	return record, nil
}

func (e *Engine) Finish(operationID string, status Status, terminalCode string) (Record, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if status != StatusSucceeded && status != StatusFailed && status != StatusCancelled {
		return Record{}, errors.New("finish status must be succeeded, failed, or cancelled")
	}
	if !stableToken.MatchString(terminalCode) {
		return Record{}, errors.New("terminal code must be a stable lowercase token")
	}

	group, record, found := findActive(e.snapshot, operationID)
	if !found {
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, operationID)
	}
	finishedAt := e.now().UTC()
	record.Status = status
	record.FinishedAt = &finishedAt
	record.TerminalCode = terminalCode

	next := cloneSnapshot(e.snapshot)
	delete(next.Active, group)
	next.Recent = append(next.Recent, record)
	if len(next.Recent) > e.recentLimit {
		next.Recent = append([]Record(nil), next.Recent[len(next.Recent)-e.recentLimit:]...)
	}
	if err := e.file.Save(next); err != nil {
		return Record{}, err
	}
	e.snapshot = next
	return record, nil
}

func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneSnapshot(e.snapshot)
}

func validateSnapshot(snapshot Snapshot) error {
	if snapshot.SchemaVersion != StateSchemaVersion {
		return fmt.Errorf("schema_version must be %d", StateSchemaVersion)
	}
	if snapshot.Active == nil || snapshot.Recent == nil {
		return errors.New("active and recent collections are required")
	}
	seen := map[string]struct{}{}
	for group, record := range snapshot.Active {
		if record.ExclusiveGroup != group || record.Status != StatusRunning || record.FinishedAt != nil || record.TerminalCode != "" {
			return fmt.Errorf("invalid active record %s", record.OperationID)
		}
		if err := validateRecordTokens(record); err != nil {
			return err
		}
		if _, exists := seen[record.OperationID]; exists {
			return fmt.Errorf("duplicate operation id %s", record.OperationID)
		}
		seen[record.OperationID] = struct{}{}
	}
	for _, record := range snapshot.Recent {
		if record.Status != StatusSucceeded && record.Status != StatusFailed && record.Status != StatusCancelled {
			return fmt.Errorf("invalid recent status for %s", record.OperationID)
		}
		if record.FinishedAt == nil || !stableToken.MatchString(record.TerminalCode) {
			return fmt.Errorf("invalid recent terminal record %s", record.OperationID)
		}
		if err := validateRecordTokens(record); err != nil {
			return err
		}
		if _, exists := seen[record.OperationID]; exists {
			return fmt.Errorf("duplicate operation id %s", record.OperationID)
		}
		seen[record.OperationID] = struct{}{}
	}
	return nil
}

func validateRecordTokens(record Record) error {
	if !stableToken.MatchString(record.OperationID) || !stableToken.MatchString(record.Kind) || !stableToken.MatchString(record.ExclusiveGroup) {
		return fmt.Errorf("invalid stable token in operation %s", record.OperationID)
	}
	if record.StartedAt.IsZero() {
		return fmt.Errorf("operation %s has no start time", record.OperationID)
	}
	return nil
}

func containsOperationID(snapshot Snapshot, operationID string) bool {
	for _, record := range snapshot.Active {
		if record.OperationID == operationID {
			return true
		}
	}
	for _, record := range snapshot.Recent {
		if record.OperationID == operationID {
			return true
		}
	}
	return false
}

func findActive(snapshot Snapshot, operationID string) (string, Record, bool) {
	groups := make([]string, 0, len(snapshot.Active))
	for group := range snapshot.Active {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	for _, group := range groups {
		record := snapshot.Active[group]
		if record.OperationID == operationID {
			return group, record, true
		}
	}
	return "", Record{}, false
}

func cloneSnapshot(source Snapshot) Snapshot {
	copy := Snapshot{
		SchemaVersion: source.SchemaVersion,
		Active:        make(map[string]Record, len(source.Active)),
		Recent:        make([]Record, len(source.Recent)),
	}
	copy.Recent = append(copy.Recent[:0], source.Recent...)
	for key, record := range source.Active {
		copy.Active[key] = record
	}
	return copy
}
