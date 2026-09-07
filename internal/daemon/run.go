package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/app"
	"github.com/akastrmix/akastr-agent/internal/autoupdate"
	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
	"github.com/akastrmix/akastr-agent/internal/systemdnotify"
	transportws "github.com/akastrmix/akastr-agent/internal/transport/ws"
)

type Options struct {
	ConfigPath string
	Version    string
	Reexec     func(string, string, string, int64) error
}

// Run owns the lifetime of business transport, maintenance and deployment trials.
func Run(ctx context.Context, model *app.Model, options Options) error {
	version := options.Version
	const releaseRoot = "/usr/local/lib/akastr-agent"
	credentials, err := identity.Load(model.Config.Control.CredentialFile)
	if err != nil {
		return err
	}
	if credentials.AgentID != model.Config.Node.ID {
		return errors.New("configured node ID does not match enrolled identity")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	lifecycleGate := lifecycle.New()
	maintenance := autoupdate.LoopOptions{
		ControlEndpoint: model.Config.Control.Endpoint,
		CurrentVersion:  version, ConfigurationRevision: model.Config.ConfigurationRevision,
		Credentials: credentials, ConfigPath: options.ConfigPath, ReleaseRoot: releaseRoot,
		Lifecycle: lifecycleGate, Retry: &autoupdate.RetryState{},
		CheckIdle: func() error {
			return app.CheckMaintenanceSafe(model.Config.StateFile, model.Config.IPStateFile, model.Config.RecentOperationLimit)
		},
		Reexec: options.Reexec, Logger: logger,
	}
	startupContext, startupCancel := context.WithTimeout(ctx, 5*time.Minute)
	_, err = autoupdate.ReconcileOnce(startupContext, maintenance)
	startupCancel()
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		logger.Warn("startup maintenance reconciliation failed", "code", "maintenance_reconciliation_failed")
	}
	runtime, err := app.BuildRuntime(model)
	if err != nil {
		return err
	}
	trial, err := autoupdate.LoadTrial(version, model.Config.ConfigurationRevision, releaseRoot, options.ConfigPath)
	if err != nil {
		return err
	}
	ready := make(chan struct{})
	var readyOnce sync.Once
	var readyError error
	onReady := func() error {
		readyOnce.Do(func() {
			if notifyError := systemdnotify.Ready(); notifyError != nil {
				readyError = notifyError
				return
			}
			close(ready)
		})
		return readyError
	}
	deploymentState := "current"
	var onDeploymentTrial func() error
	if trial != nil {
		deploymentState = "trial"
		onDeploymentTrial = func() error {
			result, commitError := trial.Commit()
			if result.CleanupFailed {
				logger.Warn("managed Agent release cleanup incomplete", "code", "update_cleanup_failed")
			}
			return commitError
		}
	}
	var observations transportws.ObservationSource
	if monitor := runtime.IPMonitor(); monitor != nil {
		observations = monitor
	}
	maintenanceTriggers := make(chan struct{}, 1)
	client, err := transportws.New(struct {
		Endpoint              string
		Identity              identity.Identity
		Version               string
		ConfigurationRevision int64
		Capabilities          []capability.Descriptor
		DeploymentState       string
		Executor              transportws.Executor
		Observations          transportws.ObservationSource
		Lifecycle             *lifecycle.Gate
		OnReady               func() error
		OnDeploymentTrial     func() error
		OnMaintenanceCheck    func()
		Logger                *slog.Logger
	}{
		Endpoint: model.Config.Control.Endpoint, Identity: credentials,
		Version: version, ConfigurationRevision: model.Config.ConfigurationRevision,
		Capabilities: model.Capabilities.List(), DeploymentState: deploymentState,
		Executor: runtime, Observations: observations,
		Lifecycle: lifecycleGate, OnReady: onReady, OnDeploymentTrial: onDeploymentTrial,
		OnMaintenanceCheck: func() {
			select {
			case maintenanceTriggers <- struct{}{}:
			default:
			}
		},
		Logger: logger,
	})
	if err != nil {
		return err
	}
	runContext, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	// A current deployment is a running service even when business WSS is incompatible.
	// A trial still has to prove business readiness before systemd accepts it.
	if trial == nil {
		if err := systemdnotify.Ready(); err != nil {
			return err
		}
	}
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		(autoupdate.Client{}).Watch(runContext, model.Config.Control.Endpoint, version,
			model.Config.ConfigurationRevision, credentials, maintenanceTriggers)
	}()
	defer func() { cancelRun(); <-watchDone }()
	updateDone := make(chan error, 1)
	controlDone := make(chan error, 1)
	var trialExpired <-chan time.Time
	if trial != nil {
		trialTimer := time.NewTimer(autoupdate.TrialReadinessTimeout)
		defer trialTimer.Stop()
		trialExpired = trialTimer.C
		go func() {
			select {
			case <-ready:
				if !trialTimer.Stop() {
					select {
					case <-trialTimer.C:
					default:
					}
				}
			case <-runContext.Done():
			}
		}()
	}
	maintenance.Ready, maintenance.Triggers = ready, maintenanceTriggers
	go func() { updateDone <- autoupdate.RunLoop(runContext, maintenance) }()
	go func() { controlDone <- client.Run(runContext) }()
	var firstError error
	select {
	case <-trialExpired:
		cancelRun()
		<-updateDone
		<-controlDone
		trialError := errors.New("automatic update trial did not reach control readiness")
		if discardError := trial.Discard(); discardError != nil {
			return errors.Join(trialError, fmt.Errorf("discard timed out automatic update trial: %w", discardError))
		}
		return trialError
	case firstError = <-updateDone:
		cancelRun()
		<-controlDone
	case firstError = <-controlDone:
		cancelRun()
		<-updateDone
	}
	if ctx.Err() != nil || errors.Is(firstError, context.Canceled) {
		return nil
	}
	return firstError
}
