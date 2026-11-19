package changeip

import (
	"context"
	"time"
)

type TriggerState string

const (
	TriggerConfirmed TriggerState = "confirmed"
	TriggerUnknown   TriggerState = "unknown"
	TriggerFailed    TriggerState = "failed"
)

type Result struct {
	State    TriggerState
	Code     string
	ExitCode int
	// Reply is what the provider answered when it refused, for an administrator
	// to read; empty when there is nothing to show.
	Reply      string
	StartedAt  time.Time
	FinishedAt time.Time
}

type Provider interface {
	Run(context.Context) Result
}
