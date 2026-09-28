package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/akastrmix/akastr-agent/internal/app"
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/layout"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
	"github.com/akastrmix/akastr-agent/internal/systemdnotify"
	transportws "github.com/akastrmix/akastr-agent/internal/transport/ws"
	"github.com/akastrmix/akastr-agent/internal/update"
)

// CandidateTimeout bounds how long an update candidate may take to be accepted
// by Cloud. When it expires the candidate exits and systemd restarts the active
// slot, which is still the previous deployment.
const CandidateTimeout = 45 * time.Second

type Options struct {
	ConfigPath string
	Version    string
	Layout     layout.Layout
	Exec       func(binary, configPath string) error
	Logger     *slog.Logger
}

// Run owns the business connection, the updater and, for a candidate, its
// activation.
func Run(ctx context.Context, options Options) error {
	paths := options.Layout
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	model, err := app.Load(options.ConfigPath)
	if err != nil {
		return err
	}
	cfg := model.Config
	credentials, err := identity.Load(paths.IdentityFile)
	if err != nil {
		return err
	}
	if credentials.AgentID != cfg.AgentID {
		return errors.New("configured node ID does not match the local identity")
	}
	slot, candidate, err := paths.Candidate(options.ConfigPath)
	if err != nil {
		return err
	}
	target := update.TargetName(options.Version, cfg.ConfigurationRevision)
	failCandidate := func(code string) {
		if candidate && update.RecordCandidateFailure(paths, target, code) != nil {
			logger.Error("update candidate failure was not recorded", "code", code)
		}
	}
	gate := lifecycle.New()
	nudges := make(chan struct{}, 1)
	nudge := func() {
		select {
		case nudges <- struct{}{}:
		default:
		}
	}
	updater := &update.Updater{
		ControlEndpoint: cfg.ControlEndpoint, Identity: credentials,
		Version: options.Version, Revision: cfg.ConfigurationRevision,
		ConfigPath: options.ConfigPath, Layout: paths, Lifecycle: gate,
		CheckSafe: func() error { return app.CheckUpdateSafe(paths) },
		Exec:      options.Exec, Logger: logger,
	}
	runtime, err := app.BuildRuntime(model, paths)
	if err != nil {
		if candidate {
			failCandidate("candidate_runtime_invalid")
			return err
		}
		// Keep updating so a release or configuration change can repair the node.
		logger.Error("local runtime unavailable; updates remain active", "code", "runtime_initialization_failed", "error", err.Error())
		if err := systemdnotify.Ready(); err != nil {
			return err
		}
		return updater.Loop(ctx, nudges)
	}
	committed := make(chan struct{})
	if !candidate {
		close(committed)
	}
	onReady := func() error {
		select {
		case <-committed:
		default:
			// The candidate is accepted by Cloud; make it the deployment systemd
			// restarts before it handles any business message.
			if err := paths.Activate(slot); err != nil {
				failCandidate("candidate_activation_failed")
				return err
			}
			close(committed)
			logger.Info("Agent update activated", "target", target)
		}
		nudge()
		return nil
	}
	var observations transportws.ObservationSource
	if monitor := runtime.IPMonitor(); monitor != nil {
		observations = monitor
	}
	client, err := transportws.New(transportws.Options{
		Endpoint: cfg.ControlEndpoint, Identity: credentials,
		Version: options.Version, ConfigurationRevision: cfg.ConfigurationRevision,
		Capabilities: model.Capabilities.List(), Executor: runtime, Observations: observations,
		Lifecycle: gate, OnReady: onReady, OnSessionEnd: nudge, Logger: logger,
	})
	if err != nil {
		return err
	}
	if err := systemdnotify.Ready(); err != nil {
		return err
	}
	deadline := time.Duration(0)
	if candidate {
		deadline = CandidateTimeout
	}
	err = supervise(ctx, client.Run, func(ctx context.Context) error { return updater.Loop(ctx, nudges) }, committed, deadline)
	if errors.Is(err, errCandidateTimeout) {
		failCandidate("candidate_not_ready")
	}
	return err
}

var errCandidateTimeout = errors.New("Agent update candidate was not accepted by Cloud in time")

// supervise runs the control connection and the updater until either stops.
// The updater starts only once the deployment is committed, so a candidate
// never updates itself; an uncommitted candidate stops at its deadline.
func supervise(ctx context.Context, control, updates func(context.Context) error, committed <-chan struct{}, deadline time.Duration) error {
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- control(runContext) }()
	go func() {
		select {
		case <-committed:
			done <- updates(runContext)
		case <-runContext.Done():
			done <- runContext.Err()
		}
	}()
	var expired <-chan time.Time
	if deadline > 0 {
		timer := time.NewTimer(deadline)
		defer timer.Stop()
		expired = timer.C
	}
	running := 2
	stop := func() {
		cancel()
		for ; running > 0; running-- {
			<-done
		}
	}
	activated := committed
	for {
		select {
		case <-activated:
			activated, expired = nil, nil
		case <-expired:
			// Activation may have happened at the same moment as the deadline.
			select {
			case <-committed:
				activated, expired = nil, nil
				continue
			default:
			}
			stop()
			return errCandidateTimeout
		case err := <-done:
			running--
			stop()
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
	}
}
