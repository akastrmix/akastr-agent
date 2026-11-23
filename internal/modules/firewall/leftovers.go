package firewall

import (
	"context"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/state"
)

const leftoverRetry = 30 * time.Second

// Leftovers removes the table and the state directory on a node whose module
// is off, and turns IPv4 forwarding back off if this Agent turned it on: the
// module never removes its table when the process stops.
func Leftovers(stateDir string) *module.Cleanup {
	return module.NewCleanup("firewall table", leftoverRetry, func(ctx context.Context) error {
		return cleanup(ctx, newSystem(""), stateDir)
	})
}

func cleanup(ctx context.Context, s *system, stateDir string) error {
	recorded := emptyState()
	found, err := state.NewJSONFile(filepath.Join(StateDirectory(stateDir), "state.json")).Load(&recorded)
	known := err == nil && found && recorded.validate() == nil
	// Forwarding this Agent turned on goes back off before its record goes.
	if known && recorded.ForwardingOpened != "" && recorded.ForwardingOpened == s.boot() {
		if err := s.setForwarding(ctx, false); err != nil {
			return err
		}
	}
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
	if known {
		// Without the table, connections it translated would keep going to
		// their target until they end. Best effort: the record goes next.
		for _, forward := range append(recorded.Forwards, recorded.Stale...) {
			to := netip.AddrPortFrom(netip.MustParseAddr(forward.To), uint16(forward.ToPort))
			if err := s.forget(ctx, forward.Protocol, forward.Port, to); err != nil {
				slog.Warn("connections of a forward not cleaned", "port", forward.Port, "error", err.Error())
			}
		}
	}
	return os.RemoveAll(StateDirectory(stateDir))
}
