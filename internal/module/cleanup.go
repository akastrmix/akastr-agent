package module

import (
	"context"
	"log/slog"
	"time"
)

// Cleanup is a Worker that removes what an earlier process left outside it,
// for a module that is off: an update replaces the process without stopping
// it, and a crash never stops it. A failed cleanup is retried every retry;
// once one succeeds, Done closes and the worker waits for the process to end.
type Cleanup struct {
	leftovers string
	retry     time.Duration
	clean     func(context.Context) error
	done      chan struct{}
}

var _ Worker = (*Cleanup)(nil)

// NewCleanup runs clean until it succeeds; leftovers names what it removes in
// the log.
func NewCleanup(leftovers string, retry time.Duration, clean func(context.Context) error) *Cleanup {
	return &Cleanup{leftovers: leftovers, retry: retry, clean: clean, done: make(chan struct{})}
}

// Done closes once the cleanup succeeded.
func (c *Cleanup) Done() <-chan struct{} { return c.done }

func (c *Cleanup) Run(ctx context.Context) error {
	for {
		err := c.clean(ctx)
		if err == nil {
			close(c.done)
			<-ctx.Done()
			return ctx.Err()
		}
		if ctx.Err() == nil {
			slog.Warn("leftovers of an earlier process remain; retrying", "leftovers", c.leftovers, "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.retry):
		}
	}
}
