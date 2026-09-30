// Package module defines how a node capability plugs into the Agent. Each
// capability is a module under internal/modules, holding all of its code, that
// Cloud switches on by including its configuration section; internal/app wires
// the enabled modules.
package module

import (
	"context"
	"encoding/json"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

// Commands handles one command type that Cloud may offer. Execution, journal
// and replay are owned by internal/operation; a module only runs or recovers.
type Commands interface {
	CommandType() string
	ExclusiveGroup() string
	// Validate rejects a malformed payload before the offer is accepted.
	Validate(payload json.RawMessage) error
	// Accepting reports whether a new command may be accepted now.
	Accepting() bool
	Run(context.Context, protocol.OperationOffer) protocol.ExecutionResult
	// Recover finishes a command whose process stopped while it was running,
	// without repeating its side effect.
	Recover(protocol.OperationOffer) protocol.ExecutionResult
}

// Publish sends one report on the current control session.
type Publish func(protocol.ReportBody) error

// Reporter runs for the life of the process and sends facts it observes as
// reports, resending each until Cloud acknowledges its report_id.
type Reporter interface {
	Run(ctx context.Context, publish Publish) error
	// ControlReady wakes the reporter when a session becomes ready.
	ControlReady()
	// Acknowledge settles a stored report; false means another module sent it.
	Acknowledge(reportID string) (bool, error)
	// UpdateSafe refuses a process replacement while its work must finish first.
	UpdateSafe() error
}
