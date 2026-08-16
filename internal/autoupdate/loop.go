package autoupdate

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/operation"
)

const CheckInterval = 6 * time.Hour

type Checker interface {
	Check(context.Context, string, string, identity.Identity) (Manifest, error)
}

type LoopOptions struct {
	ControlEndpoint      string
	CurrentVersion       string
	Credentials          identity.Identity
	ConfigPath           string
	ReleaseRoot          string
	StateFile            string
	RecentOperationLimit int
	Checker              Checker
	Ticks                <-chan time.Time
	Apply                func(context.Context, ApplyOptions) error
	Reexec               func(string) error
	Logger               *slog.Logger
}

func RunLoop(ctx context.Context, options LoopOptions) error {
	if options.ControlEndpoint == "" || options.CurrentVersion == "" ||
		options.ConfigPath == "" || options.ReleaseRoot == "" || options.StateFile == "" ||
		options.RecentOperationLimit < 1 || options.Reexec == nil {
		return errors.New("automatic update loop options are incomplete")
	}
	checker := options.Checker
	if checker == nil {
		checker = Client{}
	}
	apply := options.Apply
	if apply == nil {
		apply = Apply
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ticks := options.Ticks
	var ticker *time.Ticker
	if ticks == nil {
		ticker = time.NewTicker(CheckInterval)
		defer ticker.Stop()
		ticks = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticks:
			checkContext, cancel := context.WithTimeout(ctx, 5*time.Minute)
			manifest, err := checker.Check(
				checkContext, options.ControlEndpoint, options.CurrentVersion, options.Credentials,
			)
			if err != nil {
				cancel()
				logger.Warn("automatic update check failed", "code", "update_check_failed")
				continue
			}
			if manifest.Status != "update_available" {
				cancel()
				continue
			}
			active := func() (bool, error) {
				engine, err := operation.Open(operation.Options{
					StateFile: options.StateFile, RecentLimit: options.RecentOperationLimit,
				})
				if err != nil {
					return false, err
				}
				return len(engine.Snapshot().Active) != 0, nil
			}
			isActive, err := active()
			if err != nil || isActive {
				cancel()
				continue
			}
			err = apply(checkContext, ApplyOptions{
				Manifest: manifest, ConfigPath: options.ConfigPath, ReleaseRoot: options.ReleaseRoot,
				OperationActive: active,
			})
			cancel()
			if errors.Is(err, ErrOperationActive) {
				continue
			}
			if err != nil {
				logger.Warn("automatic update apply failed", "code", "update_apply_failed")
				continue
			}
			binary := filepath.Join(
				options.ReleaseRoot, "releases", manifest.Version, "akastr-agent",
			)
			if err := options.Reexec(binary); err != nil {
				logger.Error("automatic update process replacement failed", "code", "update_reexec_failed")
			}
		}
	}
}
