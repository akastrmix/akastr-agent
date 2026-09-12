package autoupdate

import (
	"context"
	"errors"
	"time"
)

// ErrCandidateRejected denotes a verified mismatch in an immutable target.
// Candidate exit codes cannot distinguish input errors from local I/O failures.
var ErrCandidateRejected = errors.New("candidate rejected the update target")

// RetryState is bounded to the current target and owned by the serial coordinator.
// Restarting the process permits a fresh pre-trial validation after local repairs.
type RetryState struct {
	reported *MaintenanceResult
	manualID string
	target   string
	rejected bool
	delay    time.Duration
	next     time.Time
	now      func() time.Time
}

func (s *RetryState) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *RetryState) blocked(target string) string {
	if s == nil {
		return ""
	}
	if s.target != target {
		s.target, s.rejected, s.delay, s.next = target, false, 0, time.Time{}
		s.reported = nil
	}
	if s.rejected {
		return "candidate_target_rejected"
	}
	if s.clock().Before(s.next) {
		return "maintenance_retry_wait"
	}
	return ""
}

func (s *RetryState) failed(err error) {
	if s == nil {
		return
	}
	if errors.Is(err, ErrCandidateRejected) {
		s.rejected = true
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	if s.delay == 0 {
		s.delay = time.Minute
	} else {
		s.delay *= 2
	}
	if s.delay > 5*time.Minute {
		s.delay = 5 * time.Minute
	}
	s.next = s.clock().Add(s.delay)
}

func candidateCommandError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
