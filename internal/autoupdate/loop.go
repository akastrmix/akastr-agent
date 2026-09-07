package autoupdate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/bootstrap"
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
)

const (
	CheckInterval     = 6 * time.Hour
	InitialDelayMin   = time.Minute
	InitialDelayRange = 4 * time.Minute
	DefaultConfigRoot = "/var/lib/akastr-agent/configurations"
)

type MaintenanceClient interface {
	Check(context.Context, string, string, int64, identity.Identity) (Manifest, error)
	FetchConfiguration(context.Context, string, int64, identity.Identity, string) (Configuration, error)
	Report(context.Context, string, string, identity.Identity, MaintenanceResult) error
}

type LoopOptions struct {
	ControlEndpoint       string
	CurrentVersion        string
	ConfigurationRevision int64
	Credentials           identity.Identity
	ConfigPath            string
	ConfigurationRoot     string
	ReleaseRoot           string
	Lifecycle             *lifecycle.Gate
	Ready                 <-chan struct{}
	Client                MaintenanceClient
	Ticks                 <-chan time.Time
	Triggers              <-chan struct{}
	Stage                 func(context.Context, ApplyOptions) (StagedRelease, error)
	Runner                CommandRunner
	CheckIdle             func() error
	Reexec                func(string, string, string, int64) error
	InitialDelay          func() time.Duration
	Logger                *slog.Logger
	Retry                 *RetryState
}

func ReconcileOnce(ctx context.Context, options LoopOptions) (changed bool, resultErr error) {
	if options.ControlEndpoint == "" || options.CurrentVersion == "" || options.ConfigurationRevision < 1 ||
		options.ConfigPath == "" || options.ReleaseRoot == "" || options.Lifecycle == nil || options.Reexec == nil {
		return false, errors.New("automatic maintenance options are incomplete")
	}
	client := options.Client
	if client == nil {
		client = Client{}
	}
	manifest, err := client.Check(ctx, options.ControlEndpoint, options.CurrentVersion, options.ConfigurationRevision, options.Credentials)
	if err != nil {
		return false, err
	}
	if manifest.Status != "update_available" {
		return false, nil
	}
	targetVersion := manifest.Software.Version
	reportedTargetRevision := manifest.Configuration.Revision
	report := func(status, code string) {
		_ = client.Report(ctx, options.ControlEndpoint, options.CurrentVersion, options.Credentials, MaintenanceResult{
			TargetVersion: targetVersion, TargetConfigurationRevision: reportedTargetRevision,
			Status: status, ErrorCode: code,
		})
	}
	if code := options.Retry.blocked(deploymentName(targetVersion, reportedTargetRevision)); code != "" {
		status := "busy"
		if code == "candidate_target_rejected" {
			status = "suppressed"
		}
		report(status, code)
		return false, nil
	}
	defer func() {
		if resultErr != nil {
			options.Retry.failed(resultErr)
		}
	}()
	lease, acquired := options.Lifecycle.TryUpdate()
	if !acquired {
		report("busy", "maintenance_local_busy")
		return false, nil
	}
	defer lease.Release()
	if options.CheckIdle != nil {
		if err := options.CheckIdle(); err != nil {
			report("busy", "maintenance_local_busy")
			return false, nil
		}
	}
	if attempted, err := uncommittedDeploymentExists(options.ReleaseRoot, manifest.Software.Version, manifest.Configuration.Revision); err != nil {
		report("failed", "maintenance_state_invalid")
		return false, err
	} else if attempted {
		report("suppressed", "trial_suppressed_after_failure")
		return false, nil
	}

	binary, err := os.Executable()
	if err != nil {
		report("failed", "candidate_binary_invalid")
		return false, err
	}
	if manifest.Software.Status == "update_available" {
		stage := options.Stage
		if stage == nil {
			stage = Stage
		}
		staged, err := stage(ctx, ApplyOptions{
			Manifest: manifest, ConfigPath: options.ConfigPath,
			ReleaseRoot: options.ReleaseRoot, Runner: options.Runner,
		})
		if err != nil {
			report("failed", "candidate_binary_invalid")
			return false, err
		}
		expected := filepath.Join(options.ReleaseRoot, "releases", targetVersion, "akastr-agent")
		if staged.Version != targetVersion || filepath.Clean(staged.Binary) != expected {
			report("failed", "candidate_binary_invalid")
			return false, errors.New("automatic maintenance staged an unexpected release")
		}
		binary = staged.Binary
	}

	configPath := options.ConfigPath
	targetRevision := options.ConfigurationRevision
	if manifest.Configuration.Status == "update_available" {
		configuration, err := client.FetchConfiguration(ctx, options.ControlEndpoint, manifest.Configuration.Revision, options.Credentials, targetVersion)
		if err != nil {
			report("failed", "maintenance_configuration_fetch_failed")
			return false, err
		}
		configRoot := options.ConfigurationRoot
		if configRoot == "" {
			configRoot = DefaultConfigRoot
		}
		configPath, err = materializeCandidate(ctx, options.Runner, binary, configRoot, configuration, options.Credentials.AgentID)
		if err != nil {
			report("failed", "candidate_configuration_invalid")
			return false, err
		}
		targetRevision = configuration.ConfigurationRevision
	}
	deployment, err := StageDeployment(options.ReleaseRoot, targetVersion, targetRevision, configPath)
	if err != nil {
		report("failed", "candidate_deployment_invalid")
		return false, err
	}
	binary = filepath.Join(deployment, "akastr-agent")
	configPath = filepath.Join(deployment, "config", "config.json")
	if err := options.Reexec(binary, configPath, targetVersion, targetRevision); err != nil {
		report("failed", "candidate_process_replace_failed")
		return false, errors.Join(errors.New("automatic maintenance process replacement failed"), err)
	}
	return true, nil
}

func uncommittedDeploymentExists(releaseRoot, version string, revision int64) (bool, error) {
	deploymentsRoot := filepath.Join(releaseRoot, "deployments")
	current, err := safeCurrentTarget(filepath.Join(releaseRoot, "current"), deploymentsRoot)
	if err != nil {
		return false, err
	}
	target := filepath.Join(deploymentsRoot, deploymentName(version, revision))
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("automatic maintenance target deployment is unsafe")
	}
	return filepath.Clean(target) != filepath.Clean(current), nil
}

func materializeCandidate(ctx context.Context, runner CommandRunner, binary, configRoot string, configuration Configuration, agentID string) (string, error) {
	if !filepath.IsAbs(configRoot) {
		return "", errors.New("configuration root must be absolute")
	}
	if err := os.MkdirAll(configRoot, 0o700); err != nil {
		return "", err
	}
	if info, err := os.Lstat(configRoot); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return "", errors.New("configuration root is unsafe")
	}
	target := filepath.Join(configRoot, strconv.FormatInt(configuration.ConfigurationRevision, 10))
	if info, err := os.Lstat(target); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("target configuration path is unsafe")
		}
		digest := sha256.Sum256(configuration.Bootstrap)
		storedDigest, readErr := os.ReadFile(filepath.Join(target, bootstrap.ConfigurationBootstrapDigestFile))
		if readErr != nil || strings.TrimSpace(string(storedDigest)) != fmt.Sprintf("%x", digest) {
			return "", fmt.Errorf("%w: target configuration does not match desired bootstrap", ErrCandidateRejected)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	} else {
		staging, stageErr := os.MkdirTemp(configRoot, ".configuration-")
		if stageErr != nil {
			return "", stageErr
		}
		defer os.RemoveAll(staging)
		if err := os.Chmod(staging, 0o700); err != nil {
			return "", err
		}
		input := filepath.Join(staging, "bootstrap.json")
		if err := os.WriteFile(input, configuration.Bootstrap, 0o600); err != nil {
			return "", err
		}
		output := filepath.Join(staging, "materialized")
		if err := os.Mkdir(output, 0o700); err != nil {
			return "", err
		}
		if runner == nil {
			runner = systemRunner{}
		}
		if _, err := runner.Output(ctx, binary, "materialize-configuration", "--input", input, "--output-dir", output, "--runtime-dir", target, "--agent-id", agentID, "--revision", strconv.FormatInt(configuration.ConfigurationRevision, 10)); err != nil {
			return "", fmt.Errorf("candidate Agent rejected desired configuration: %w", candidateCommandError(ctx, err))
		}
		if err := os.Remove(input); err != nil {
			return "", err
		}
		if err := os.Rename(output, target); err != nil {
			return "", err
		}
		if err := syncDirectory(configRoot); err != nil {
			return "", fmt.Errorf("sync Agent configuration root: %w", err)
		}
	}
	if runner == nil {
		runner = systemRunner{}
	}
	configPath := filepath.Join(target, "config.json")
	validation, err := runner.Output(ctx, binary, "validate-configuration", "--config", configPath)
	if err != nil {
		return "", fmt.Errorf("candidate Agent runtime validation failed: %w", candidateCommandError(ctx, err))
	}
	var result struct {
		AgentID               string          `json:"agent_id"`
		ConfigurationRevision int64           `json:"configuration_revision"`
		Capabilities          json.RawMessage `json:"capabilities"`
	}
	decoder := json.NewDecoder(strings.NewReader(validation))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return "", fmt.Errorf("%w: candidate Agent validation result is invalid", ErrCandidateRejected)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("%w: candidate Agent validation result contains trailing JSON", ErrCandidateRejected)
	}
	if result.AgentID != agentID || result.ConfigurationRevision != configuration.ConfigurationRevision {
		return "", fmt.Errorf("%w: candidate Agent validation identity is inconsistent", ErrCandidateRejected)
	}
	// The approved candidate validates its own capability schema. The old updater
	// only checks the stable identity/revision envelope.
	return configPath, nil
}

func RunLoop(ctx context.Context, options LoopOptions) error {
	if options.Retry == nil {
		options.Retry = &RetryState{}
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ticks := options.Ticks
	var ticker *time.Ticker
	if ticks == nil {
		if options.Ready == nil {
			return errors.New("automatic maintenance readiness signal is required")
		}
		triggeredBeforeReady := false
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-options.Ready:
		case <-options.Triggers:
			triggeredBeforeReady = true
		}
		if !triggeredBeforeReady {
			delay := InitialDelayMin + time.Duration(rand.Int64N(int64(InitialDelayRange)+1))
			if options.InitialDelay != nil {
				delay = options.InitialDelay()
			}
			if delay < 0 {
				return errors.New("automatic maintenance initial delay is invalid")
			}
			initial := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				initial.Stop()
				return ctx.Err()
			case <-initial.C:
			case <-options.Triggers:
				if !initial.Stop() {
					select {
					case <-initial.C:
					default:
					}
				}
			}
		}
		initialTick := make(chan time.Time, 1)
		initialTick <- time.Now()
		ticker = time.NewTicker(CheckInterval)
		defer ticker.Stop()
		ticks = mergeTicks(ctx, initialTick, ticker.C)
	}
	ticks = mergeTriggers(ctx, ticks, options.Triggers)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticks:
			checkContext, cancel := context.WithTimeout(ctx, 5*time.Minute)
			_, err := ReconcileOnce(checkContext, options)
			cancel()
			if err != nil {
				logger.Warn("automatic maintenance reconciliation failed", "code", "maintenance_reconciliation_failed")
				continue
			}
		}
	}
}

func mergeTriggers(ctx context.Context, ticks <-chan time.Time, triggers <-chan struct{}) <-chan time.Time {
	if triggers == nil {
		return ticks
	}
	merged := make(chan time.Time)
	go func() {
		defer close(merged)
		for ticks != nil || triggers != nil {
			var tick time.Time
			var ok bool
			select {
			case <-ctx.Done():
				return
			case tick, ok = <-ticks:
				if !ok {
					ticks = nil
					continue
				}
			case _, ok = <-triggers:
				if !ok {
					triggers = nil
					continue
				}
				tick = time.Now()
			}
			select {
			case merged <- tick:
			case <-ctx.Done():
				return
			}
		}
	}()
	return merged
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
