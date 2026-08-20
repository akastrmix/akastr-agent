package autoupdate

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"path/filepath"
	"time"

	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
)

const (
	CheckInterval     = 6 * time.Hour
	InitialDelayMin   = time.Minute
	InitialDelayRange = 4 * time.Minute
)

type Checker interface {
	Check(context.Context, string, string, identity.Identity) (Manifest, error)
}

type LoopOptions struct {
	ControlEndpoint string
	CurrentVersion  string
	Credentials     identity.Identity
	ConfigPath      string
	ReleaseRoot     string
	Lifecycle       *lifecycle.Gate
	Ready           <-chan struct{}
	Checker         Checker
	Ticks           <-chan time.Time
	Stage           func(context.Context, ApplyOptions) (StagedRelease, error)
	Reexec          func(string, string) error
	InitialDelay    func() time.Duration
	Logger          *slog.Logger
}

func RunLoop(ctx context.Context, options LoopOptions) error {
	if options.ControlEndpoint == "" || options.CurrentVersion == "" ||
		options.ConfigPath == "" || options.ReleaseRoot == "" ||
		options.Lifecycle == nil || options.Reexec == nil {
		return errors.New("automatic update loop options are incomplete")
	}
	checker := options.Checker
	if checker == nil {
		checker = Client{}
	}
	stage := options.Stage
	if stage == nil {
		stage = Stage
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ticks := options.Ticks
	var ticker *time.Ticker
	if ticks == nil {
		if options.Ready == nil {
			return errors.New("automatic update readiness signal is required")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-options.Ready:
		}
		delay := InitialDelayMin + time.Duration(rand.Int64N(int64(InitialDelayRange)+1))
		if options.InitialDelay != nil {
			delay = options.InitialDelay()
		}
		if delay < 0 {
			return errors.New("automatic update initial delay is invalid")
		}
		initial := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			initial.Stop()
			return ctx.Err()
		case <-initial.C:
		}
		initialTick := make(chan time.Time, 1)
		initialTick <- time.Now()
		ticker = time.NewTicker(CheckInterval)
		defer ticker.Stop()
		ticks = mergeTicks(ctx, initialTick, ticker.C)
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
			lease, acquired := options.Lifecycle.TryUpdate()
			if !acquired {
				cancel()
				continue
			}
			staged, err := stage(checkContext, ApplyOptions{
				Manifest: manifest, ConfigPath: options.ConfigPath, ReleaseRoot: options.ReleaseRoot,
			})
			cancel()
			if err != nil {
				lease.Release()
				logger.Warn("automatic update stage failed", "code", "update_stage_failed")
				continue
			}
			binary := filepath.Clean(staged.Binary)
			expected := filepath.Join(options.ReleaseRoot, "releases", manifest.Version, "akastr-agent")
			if staged.Version != manifest.Version || binary != expected {
				lease.Release()
				return errors.New("automatic update stage returned an unexpected release")
			}
			reexecError := options.Reexec(binary, manifest.Version)
			lease.Release()
			if reexecError != nil {
				logger.Error("automatic update process replacement failed", "code", "update_reexec_failed")
				return errors.Join(errors.New("automatic update process replacement failed"), reexecError)
			}
		}
	}
}

func mergeTicks(ctx context.Context, first <-chan time.Time, later <-chan time.Time) <-chan time.Time {
	merged := make(chan time.Time)
	go func() {
		defer close(merged)
		for first != nil || later != nil {
			select {
			case <-ctx.Done():
				return
			case tick, ok := <-first:
				if !ok {
					first = nil
					continue
				}
				select {
				case merged <- tick:
				case <-ctx.Done():
					return
				}
			case tick, ok := <-later:
				if !ok {
					later = nil
					continue
				}
				select {
				case merged <- tick:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return merged
}
