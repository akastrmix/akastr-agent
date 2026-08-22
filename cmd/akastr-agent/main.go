package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/akastrmix/akastr-agent/internal/app"
	"github.com/akastrmix/akastr-agent/internal/autoupdate"
	"github.com/akastrmix/akastr-agent/internal/bootstrap"
	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/features/ipwatch"
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
	"github.com/akastrmix/akastr-agent/internal/operation"
	"github.com/akastrmix/akastr-agent/internal/systemdnotify"
	transportws "github.com/akastrmix/akastr-agent/internal/transport/ws"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "akastr-agent:", err)
		exitCode := 1
		if errors.Is(err, identity.ErrEnrollmentRejected) {
			exitCode = 20
		} else if errors.Is(err, identity.ErrEnrollmentOutcomeUncertain) {
			exitCode = 21
		}
		os.Exit(exitCode)
	}
}

func run(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("expected one of: bootstrap, materialize-configuration, validate-configuration, run, enroll, check-config, check-idle, capabilities, version")
	}
	switch arguments[0] {
	case "version":
		if len(arguments) != 1 {
			return errors.New("version accepts no arguments")
		}
		_, err := fmt.Fprintln(output, version)
		return err
	case "bootstrap":
		flags := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		agentID := flags.String("agent-id", "", "persistent node UUID")
		endpoint := flags.String("endpoint", "", "HTTPS bootstrap endpoint")
		tokenFile := flags.String("token-file", "", "root-only machine token file")
		outputDir := flags.String("output-dir", "", "empty root-only output directory")
		configurationRoot := flags.String("configuration-root", "", "managed configuration revision root")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		payload, err := bootstrap.FetchAndWrite(ctx, bootstrap.FetchOptions{
			Endpoint: *endpoint, AgentID: *agentID, TokenFile: *tokenFile,
			OutputDir: *outputDir, ConfigurationRoot: *configurationRoot, IPQVersion: bootstrap.IPQualityVersion,
			IPQSHA256: bootstrap.IPQualitySHA256,
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "bootstrap_mode=%s\n", payload.Mode)
		return err
	case "materialize-configuration":
		flags := flag.NewFlagSet("materialize-configuration", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		inputPath := flags.String("input", "", "root-only bootstrap payload file")
		outputDir := flags.String("output-dir", "", "empty root-only configuration directory")
		runtimeDir := flags.String("runtime-dir", "", "final managed configuration directory")
		agentID := flags.String("agent-id", "", "persistent node UUID")
		revision := flags.Int64("revision", 0, "desired configuration revision")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *inputPath == "" || *runtimeDir == "" {
			return errors.New("materialize-configuration arguments are incomplete")
		}
		info, err := os.Stat(*inputPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return errors.New("configuration input must be a root-only regular file")
		}
		raw, err := os.ReadFile(*inputPath)
		if err != nil {
			return err
		}
		if len(raw) == 0 || len(raw) > 128*1024 {
			return errors.New("configuration input size is invalid")
		}
		payload, err := bootstrap.MaterializeConfiguration(*outputDir, *runtimeDir, raw, *agentID, *revision)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "configuration_revision=%d\n", payload.ConfigurationRevision)
		return err
	case "run", "enroll", "check-config", "check-idle", "capabilities", "validate-configuration":
		flags := flag.NewFlagSet(arguments[0], flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		configPath := flags.String("config", "/etc/akastr-agent/config.json", "configuration file")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		model, err := app.Load(*configPath)
		if err != nil {
			return err
		}
		if arguments[0] == "validate-configuration" {
			if _, err := app.BuildRuntime(model); err != nil {
				return fmt.Errorf("validate runtime dependencies: %w", err)
			}
			return json.NewEncoder(output).Encode(struct {
				AgentID               string                  `json:"agent_id"`
				ConfigurationRevision int64                   `json:"configuration_revision"`
				Capabilities          []capability.Descriptor `json:"capabilities"`
			}{
				AgentID: model.Config.Node.ID, ConfigurationRevision: model.Config.ConfigurationRevision,
				Capabilities: model.Capabilities.List(),
			})
		}
		if arguments[0] == "check-config" {
			if _, err := app.BuildRuntime(model); err != nil {
				return fmt.Errorf("validate runtime dependencies: %w", err)
			}
			_, err := fmt.Fprintln(output, "configuration valid")
			return err
		}
		if arguments[0] == "check-idle" {
			if err := checkIdle(model.Config.StateFile, model.Config.IPStateFile, model.Config.RecentOperationLimit); err != nil {
				return err
			}
			_, err = fmt.Fprintln(output, "Agent is idle")
			return err
		}
		if arguments[0] == "enroll" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			created, err := identity.Enroll(ctx, struct {
				Endpoint              string
				TokenFile             string
				IdentityFile          string
				ExpectedAgentID       string
				AgentVersion          string
				ConfigurationRevision int64
				Capabilities          []capability.Descriptor
				HTTPClient            *http.Client
			}{
				Endpoint:              model.Config.Control.Endpoint,
				TokenFile:             model.Config.Control.MachineTokenFile,
				IdentityFile:          model.Config.Control.CredentialFile,
				ExpectedAgentID:       model.Config.Node.ID,
				AgentVersion:          version,
				ConfigurationRevision: model.Config.ConfigurationRevision,
				Capabilities:          model.Capabilities.List(),
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(output, "enrolled agent %s\n", created.AgentID)
			return err
		}
		if arguments[0] == "run" {
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
			startupContext, startupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
			_, err = autoupdate.ReconcileOnce(startupContext, autoupdate.LoopOptions{
				ControlEndpoint: model.Config.Control.Endpoint,
				CurrentVersion:  version, ConfigurationRevision: model.Config.ConfigurationRevision,
				Credentials: credentials, ConfigPath: *configPath, ReleaseRoot: releaseRoot,
				Lifecycle: lifecycleGate,
				CheckIdle: func() error {
					return checkIdle(model.Config.StateFile, model.Config.IPStateFile, model.Config.RecentOperationLimit)
				},
				Reexec: reexecAgent,
				Logger: logger,
			})
			startupCancel()
			if err != nil {
				logger.Warn("startup maintenance reconciliation failed", "code", "maintenance_reconciliation_failed")
			}
			runtime, err := app.BuildRuntime(model)
			if err != nil {
				return err
			}
			trial, err := autoupdate.LoadTrial(version, model.Config.ConfigurationRevision, releaseRoot, *configPath)
			if err != nil {
				return err
			}
			ready := make(chan struct{})
			var readyOnce sync.Once
			var readyError error
			onReady := func() error {
				readyOnce.Do(func() {
					if trial != nil {
						result, commitError := trial.Commit()
						if commitError != nil {
							readyError = fmt.Errorf("commit automatic update trial: %w", commitError)
							return
						}
						if result.CleanupFailed {
							logger.Warn("old Agent release cleanup incomplete", "code", "update_cleanup_failed")
						}
					}
					if notifyError := systemdnotify.Ready(); notifyError != nil {
						readyError = notifyError
						return
					}
					close(ready)
				})
				return readyError
			}
			var observations transportws.ObservationSource
			if monitor := runtime.IPMonitor(); monitor != nil {
				observations = monitor
			}
			client, err := transportws.New(struct {
				Endpoint              string
				Identity              identity.Identity
				Version               string
				ConfigurationRevision int64
				Capabilities          []capability.Descriptor
				Executor              transportws.Executor
				Observations          transportws.ObservationSource
				Lifecycle             *lifecycle.Gate
				OnReady               func() error
				Logger                *slog.Logger
			}{
				Endpoint: model.Config.Control.Endpoint, Identity: credentials,
				Version: version, ConfigurationRevision: model.Config.ConfigurationRevision,
				Capabilities: model.Capabilities.List(),
				Executor:     runtime, Observations: observations,
				Lifecycle: lifecycleGate, OnReady: onReady, Logger: logger,
			})
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			runContext, cancelRun := context.WithCancel(ctx)
			defer cancelRun()
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
			go func() {
				updateDone <- autoupdate.RunLoop(runContext, autoupdate.LoopOptions{
					ControlEndpoint:       model.Config.Control.Endpoint,
					CurrentVersion:        version,
					ConfigurationRevision: model.Config.ConfigurationRevision,
					Credentials:           credentials,
					ConfigPath:            *configPath,
					ReleaseRoot:           releaseRoot,
					Lifecycle:             lifecycleGate,
					Ready:                 ready,
					CheckIdle: func() error {
						return checkIdle(model.Config.StateFile, model.Config.IPStateFile, model.Config.RecentOperationLimit)
					},
					Reexec: reexecAgent,
					Logger: logger,
				})
			}()
			go func() { controlDone <- client.Run(runContext) }()
			var firstError error
			select {
			case <-trialExpired:
				cancelRun()
				<-updateDone
				<-controlDone
				return errors.New("automatic update trial did not reach control readiness")
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
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(model.Capabilities.List())
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func checkIdle(stateFile, ipStateFile string, recentLimit int) error {
	engine, err := operation.Open(operation.Options{StateFile: stateFile, RecentLimit: recentLimit})
	if err != nil {
		return err
	}
	if len(engine.Snapshot().Active) != 0 {
		return errors.New("an Agent operation is active")
	}
	return ipwatch.CheckIdle(ipStateFile)
}
