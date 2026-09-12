package autoupdate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	Client                MaintenanceClient
	Triggers              <-chan Trigger
	Stage                 func(context.Context, ApplyOptions) (StagedRelease, error)
	Runner                CommandRunner
	CheckIdle             func() error
	Reexec                func(string, string, string, int64) error
	Logger                *slog.Logger
	Retry                 *RetryState
}

func ReconcileOnce(ctx context.Context, options LoopOptions) (changed bool, resultErr error) {
	if options.ControlEndpoint == "" || options.CurrentVersion == "" || options.ConfigurationRevision < 1 ||
		options.ConfigPath == "" || options.ReleaseRoot == "" || options.Lifecycle == nil || options.Reexec == nil {
		return false, errors.New("automatic maintenance options are incomplete")
	}
	if options.Retry == nil {
		options.Retry = &RetryState{}
	}
	defer func() {
		if resultErr != nil {
			options.Retry.failed(resultErr)
		}
	}()
	lock, err := lockMaintenance(options.ReleaseRoot)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	configRoot := options.ConfigurationRoot
	if configRoot == "" {
		configRoot = DefaultConfigRoot
	}
	cleanupWarning := func(err error) {
		if err != nil {
			logger := options.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("managed Agent artifact cleanup incomplete", "code", "update_cleanup_failed")
		}
	}
	cleanupWarning(cleanupStaging(options.ReleaseRoot, configRoot))
	client := options.Client
	if client == nil {
		client = Client{}
	}
	manifest, err := client.Check(ctx, options.ControlEndpoint, options.CurrentVersion, options.ConfigurationRevision, options.Credentials)
	if err != nil {
		return false, err
	}
	if manifest.Status == "current" {
		cleanupWarning(cleanupCandidates(options.ReleaseRoot, configRoot, "", 0))
	}
	if manifest.Status != "update_available" {
		if manifest.Status == "current" {
			*options.Retry = RetryState{now: options.Retry.now}
		} else {
			options.Retry.next = options.Retry.clock().Add(30 * time.Second)
		}
		return false, nil
	}
	targetVersion := manifest.Software.Version
	reportedTargetRevision := manifest.Configuration.Revision
	report := func(status, code string) {
		result := MaintenanceResult{
			TargetVersion: targetVersion, TargetConfigurationRevision: reportedTargetRevision,
			Status: status, ErrorCode: code,
		}
		if options.Retry.reported != nil && *options.Retry.reported == result {
			return
		}
		if client.Report(ctx, options.ControlEndpoint, options.CurrentVersion, options.Credentials, result) == nil {
			options.Retry.reported = &result
		}
	}
	cleanupWarning(cleanupCandidates(options.ReleaseRoot, configRoot, targetVersion, reportedTargetRevision))
	ledger, manual, err := loadAttempts(options.ReleaseRoot, targetVersion, reportedTargetRevision, options.Retry.manualID)
	if err != nil {
		report("failed", "maintenance_state_invalid")
		return false, err
	}
	options.Retry.manualID = ""
	if manual {
		*options.Retry = RetryState{}
	}
	if ledger.record.Attempts >= 2 {
		report("suppressed", "trial_suppressed_after_failure")
		return false, nil
	}
	if code := options.Retry.blocked(deploymentName(targetVersion, reportedTargetRevision)); code != "" {
		status := "busy"
		if code == "candidate_target_rejected" {
			status = "suppressed"
		}
		report(status, code)
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
	// Candidate preparation is immutable and does not own the running service.
	// Reserve execution only for the final target check and process replacement.
	lease, acquired := options.Lifecycle.TryUpdate()
	if !acquired {
		report("busy", "maintenance_local_busy")
		options.Retry.next = options.Retry.clock().Add(30 * time.Second)
		return false, nil
	}
	defer lease.Release()
	if options.CheckIdle != nil {
		if err := options.CheckIdle(); err != nil {
			report("busy", "maintenance_local_busy")
			options.Retry.next = options.Retry.clock().Add(30 * time.Second)
			return false, nil
		}
	}
	latest, err := client.Check(ctx, options.ControlEndpoint, options.CurrentVersion, options.ConfigurationRevision, options.Credentials)
	if err != nil {
		return false, err
	}
	if latest.Status != "update_available" || latest.Software != manifest.Software || latest.Configuration != manifest.Configuration {
		options.Retry.next = options.Retry.clock().Add(30 * time.Second)
		return false, nil
	}
	if err := ledger.begin(); err != nil {
		report("failed", "maintenance_state_invalid")
		return false, err
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
		if readErr != nil {
			return "", readErr
		}
		if strings.TrimSpace(string(storedDigest)) != fmt.Sprintf("%x", digest) {
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

// RunLoop has one deadline for periodic work, retry and coalesced notifications.
// A one-second window combines startup/connection bursts; explicit manual grants
// run immediately. Network failures retry even if the notification channel is down.
func RunLoop(ctx context.Context, options LoopOptions) error {
	if options.Retry == nil {
		options.Retry = &RetryState{}
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	triggers := options.Triggers
	nextPeriodic := time.Now().Add(time.Second)
	var pending time.Time
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		next := nextPeriodic
		for _, deadline := range []time.Time{pending, options.Retry.next} {
			if !deadline.IsZero() && deadline.Before(next) {
				next = deadline
			}
		}
		timer.Reset(time.Until(next))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case trigger, ok := <-triggers:
			if !ok {
				triggers = nil
				continue
			}
			if trigger.RetryID != "" {
				options.Retry.manualID = trigger.RetryID
				pending = time.Now()
			} else if pending.IsZero() {
				pending = time.Now().Add(time.Second)
			}
			continue
		case <-timer.C:
		}
		now := time.Now()
		if !now.Before(nextPeriodic) {
			nextPeriodic = now.Add(CheckInterval)
		}
		// Work due within the coalescing window is covered by this check. Notifications
		// arriving during the check remain queued so a concurrent target change is seen.
		if !pending.After(now.Add(time.Second)) {
			pending = time.Time{}
		}
		checkContext, cancel := context.WithTimeout(ctx, 5*time.Minute)
		_, err := ReconcileOnce(checkContext, options)
		cancel()
		if !options.Retry.next.After(time.Now()) {
			options.Retry.next = time.Time{}
		}
		if err != nil {
			logger.Warn("automatic maintenance reconciliation failed", "code", "maintenance_reconciliation_failed")
		}
	}
}

type Trigger struct{ RetryID string }

// Coalesce notifications, but never replace a queued manual grant with a poll.
func Notify(triggers chan Trigger, trigger Trigger) {
	for {
		select {
		case triggers <- trigger:
			return
		default:
		}
		if trigger.RetryID == "" {
			return
		}
		select {
		case pending := <-triggers:
			if pending.RetryID != "" {
				trigger = pending
			}
		default:
		}
	}
}
