package app

import (
	"errors"

	"github.com/akastrmix/akastr-agent/internal/features/ipwatch"
	"github.com/akastrmix/akastr-agent/internal/layout"
	"github.com/akastrmix/akastr-agent/internal/operation"
)

// CheckUpdateSafe refuses to replace the process while an operation or a
// ChangeIP reconciliation is in flight. Pending IP facts are durable and are
// replayed by the next process, so they do not block an update.
func CheckUpdateSafe(paths layout.Layout) error {
	engine, err := operation.Open(operation.Options{StateFile: paths.StateFile(), RecentLimit: recentOperationLimit})
	if err != nil {
		return err
	}
	if len(engine.Snapshot().Active) != 0 {
		return errors.New("an Agent operation is active")
	}
	return ipwatch.CheckMaintenanceSafe(paths.IPStateFile())
}
