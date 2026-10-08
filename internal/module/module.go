// Package module defines how a node capability plugs into the Agent. Each
// capability is a module under internal/modules, holding all of its code, that
// Cloud switches on by including its configuration section; internal/app wires
// the enabled modules. A module takes any of three shapes:
//
//   - Commands: one-off work that must not run twice, such as ChangeIP;
//   - Desired: Cloud-owned targets the module converges to and may reapply;
//   - Reporter: facts the node observes and reports on its own.
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

// Desired handles a module whose Cloud-owned settings are keyed targets that
// Cloud replaces as a whole. internal/desired keeps the targets and decides
// when to apply them; applying the same targets again must change nothing.
type Desired interface {
	// Validate rejects a malformed target before it replaces the previous one.
	Validate(key string, state json.RawMessage) error
	// Apply converges the node to every current target of the module, removing
	// whatever the module owns that no key names. A nil target names a key
	// whose target is unusable: the module leaves what that key covers as it
	// is. Apply returns a stable error code for each key not in its target (a
	// missing key succeeded) and an error when work outside any key, such as a
	// removal, failed and must be retried.
	Apply(ctx context.Context, targets map[string]json.RawMessage) (map[string]string, error)
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
