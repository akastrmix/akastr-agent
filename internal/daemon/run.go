package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"

	"github.com/akastrmix/akastr-agent/internal/app"
	"github.com/akastrmix/akastr-agent/internal/autoupdate"
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
	return run(ctx, model, options, "/usr/local/lib/akastr-agent")
}

func run(ctx context.Context, model *app.Model, options Options, releaseRoot string) error {
	version := options.Version
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
	trial, err := autoupdate.LoadTrial(version, model.Config.ConfigurationRevision, releaseRoot, options.ConfigPath)
	if err != nil {
		return err
	}
	maintenanceTriggers := make(chan autoupdate.Trigger, 1)
	maintenance.Triggers = maintenanceTriggers
	services := serviceGroup{
		notify:      systemdnotify.Ready,
		maintenance: func(ctx context.Context) error { return autoupdate.RunLoop(ctx, maintenance) },
		watch: func(ctx context.Context) {
			(autoupdate.Client{}).Watch(ctx, model.Config.Control.Endpoint, version,
				model.Config.ConfigurationRevision, credentials, maintenanceTriggers)
		},
		trialTimeout: autoupdate.TrialReadinessTimeout,
	}
	runtime, err := app.BuildRuntime(model)
	if err != nil {
		if trial != nil {
			return err
		}
		logger.Error("local runtime unavailable; HTTPS maintenance remains active", "code", "runtime_initialization_failed")
		return services.run(ctx)
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
	client, err := transportws.New(transportws.Options{
		Endpoint: model.Config.Control.Endpoint, Identity: credentials,
		Version: version, ConfigurationRevision: model.Config.ConfigurationRevision,
		Capabilities: model.Capabilities.List(), DeploymentState: deploymentState,
		Executor: runtime, Observations: observations,
		Lifecycle: lifecycleGate, OnReady: onReady, OnDeploymentTrial: onDeploymentTrial,
		OnMaintenanceCheck: func(retryID string) {
			autoupdate.Notify(maintenanceTriggers, autoupdate.Trigger{RetryID: retryID})
		},
		Logger: logger,
	})
	if err != nil {
		return err
	}
	services.control, services.ready = client.Run, ready
	if trial != nil {
		services.discardTrial = trial.Discard
	}
	return services.run(ctx)
}
