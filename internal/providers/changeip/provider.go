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
	State      TriggerState
	Code       string
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
}

type Provider interface {
	Run(context.Context) Result
}
