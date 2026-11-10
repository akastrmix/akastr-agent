package firewall

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/akastrmix/akastr-agent/internal/module"
)

const leftoverRetry = 30 * time.Second

// Leftovers removes the table and the state directory on a node whose module
// is off: an update replaces the process without stopping it, and the module
// never removes its table when the process stops.
type Leftovers struct{ StateDir string }

var _ module.Worker = Leftovers{}

func (l Leftovers) Run(ctx context.Context) error {
	for {
		err := cleanup(ctx, newSystem(""), l.StateDir)
		if err == nil {
			<-ctx.Done()
			return ctx.Err()
		}
		if ctx.Err() == nil {
			slog.Warn("firewall table of an earlier process remains; retrying", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(leftoverRetry):
		}
	}
}

func cleanup(ctx context.Context, s *system, stateDir string) error {
	if !toolMissing() {
		installed, err := s.installed(ctx)
		if err != nil {
			return err
		}
		if installed != "" {
			if err := s.load(ctx, remove()); err != nil {
				return err
			}
		}
	}
	return os.RemoveAll(StateDirectory(stateDir))
}
