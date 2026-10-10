package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/layout"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
	"github.com/akastrmix/akastr-agent/internal/state"
)

const (
	checkInterval  = time.Minute
	retryInterval  = 10 * time.Second
	busyInterval   = 15 * time.Second
	maxBinaryBytes = 32 * 1024 * 1024
)

// CommandRunner runs the candidate binary's stable CLI: `version` and
// `prepare --config --agent-id --revision`.
type CommandRunner func(ctx context.Context, binary string, args ...string) (string, error)

type Updater struct {
	ControlEndpoint string
	Identity        identity.Identity
	Version         string
	Revision        int64
	// ConfigPath is the running configuration, reused when only software changes.
	ConfigPath string
	Layout     layout.Layout
	Lifecycle  *lifecycle.Gate
	// CheckSafe refuses to replace the process during work that must finish first.
	CheckSafe func() error
	// Exec replaces the running process with the prepared candidate.
	Exec     func(binary, configPath string) error
	Client   Client
	Download *http.Client
	Run      CommandRunner
	Logger   *slog.Logger
	Now      func() time.Time

	lastError string
}

// Loop checks at a fixed interval and whenever nudged. The daemon nudges after
// every control connection change, which is when Cloud restarts with a new
// release or disconnects a node whose configuration changed.
func (u *Updater) Loop(ctx context.Context, nudges <-chan struct{}) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-nudges:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
		timer.Reset(u.check(ctx))
	}
}

func (u *Updater) check(ctx context.Context) time.Duration {
	// Only the check is bounded as a whole; a download is bounded by stalls.
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	target, err := u.Client.Check(checkCtx, u.ControlEndpoint, u.Identity, u.Version, u.Revision, u.lastError)
	cancel()
	if err != nil {
		u.logger().Warn("update check failed", "code", "update_check_failed")
		return retryInterval
	}
	switch target.Status {
	case "current":
		u.lastError = ""
		return checkInterval
	case "busy":
		return busyInterval
	}
	delay, code := u.apply(ctx, target)
	if code != "" {
		u.lastError = code
		u.logger().Warn("update not applied", "code", code, "target", target.Name())
	}
	return delay
}

func (u *Updater) apply(ctx context.Context, target Target) (time.Duration, string) {
	lock, err := u.Layout.Lock()
	if errors.Is(err, layout.ErrLocked) {
		return busyInterval, ""
	}
	if err != nil {
		return checkInterval, "update_state_invalid"
	}
	defer lock.Close()
	record, err := loadAttempts(u.Layout.AttemptFile())
	if err != nil {
		return checkInterval, "update_state_invalid"
	}
	if record.Target != target.Name() {
		record = attemptRecord{Schema: 1, Target: target.Name()}
	}
	if !record.allowed(u.now()) {
		return checkInterval, record.failureCode()
	}
	// Preparing does not stop the node from taking work, so only skip it when
	// the node is visibly busy; the lease below makes the final decision.
	if u.CheckSafe != nil && u.CheckSafe() != nil {
		return busyInterval, ""
	}
	slot, code := u.prepare(ctx, target)
	if code != "" {
		return checkInterval, code
	}
	lease, acquired := u.Lifecycle.TryUpdate()
	if !acquired {
		return busyInterval, ""
	}
	defer lease.Release()
	if u.CheckSafe != nil && u.CheckSafe() != nil {
		return busyInterval, ""
	}
	record.Attempts++
	record.LastAttemptAt = u.now().UTC()
	record.LastError = ""
	if err := state.NewJSONFile(u.Layout.AttemptFile()).Save(record); err != nil {
		return checkInterval, "update_state_invalid"
	}
	u.logger().Info("starting Agent update candidate", "target", target.Name())
	if err := u.Exec(layout.SlotBinary(slot), layout.SlotConfig(slot)); err != nil {
		return checkInterval, "update_exec_failed"
	}
	return checkInterval, ""
}

// prepare writes the target into the inactive slot and lets the candidate
// binary validate its own configuration before anything is replaced.
func (u *Updater) prepare(ctx context.Context, target Target) (string, string) {
	slot, err := u.Layout.InactiveSlot()
	if err != nil {
		return "", "update_state_invalid"
	}
	binary, err := u.binary(ctx, target, layout.SlotBinary(slot))
	if err != nil {
		u.logger().Warn("update binary unavailable", "code", "update_download_failed", "error", err.Error())
		return "", "update_download_failed"
	}
	configuration := []byte(target.Configuration)
	if !target.hasConfiguration() {
		if configuration, err = os.ReadFile(u.ConfigPath); err != nil {
			return "", "update_state_invalid"
		}
	}
	if err := u.Layout.PrepareSlot(slot); err != nil ||
		layout.WriteFile(layout.SlotBinary(slot), binary, 0o755) != nil ||
		layout.WriteFile(layout.SlotConfig(slot), configuration, 0o600) != nil ||
		layout.SyncDirectory(slot) != nil {
		return "", "update_state_invalid"
	}
	run := u.Run
	if run == nil {
		run = runCommand
	}
	candidate := layout.SlotBinary(slot)
	if version, err := run(ctx, candidate, "version"); err != nil || strings.TrimSpace(version) != target.Version {
		return "", "update_candidate_invalid"
	}
	if _, err := run(ctx, candidate, "prepare", "--config", layout.SlotConfig(slot),
		"--agent-id", u.Identity.AgentID, "--revision", strconv.FormatInt(target.ConfigurationRevision, 10)); err != nil {
		u.logger().Warn("update candidate rejected its configuration", "code", "update_candidate_invalid", "error", err.Error())
		return "", "update_candidate_invalid"
	}
	return slot, ""
}

// binary reuses an identical binary already on disk before downloading one.
func (u *Updater) binary(ctx context.Context, target Target, slotBinary string) ([]byte, error) {
	running, _ := os.Executable()
	for _, candidate := range []string{slotBinary, running} {
		if contents, err := os.ReadFile(candidate); err == nil && digest(contents) == target.BinarySHA256 {
			return contents, nil
		}
	}
	client := u.Download
	if client == nil {
		// Each attempt's pool is its own; an interrupted redirect chain would
		// otherwise leave the earlier hop's connection idle until the server drops it.
		client = downloadClient()
		defer client.CloseIdleConnections()
	}
	return download(ctx, client, target.BinaryURL, "Akastr-Agent/"+u.Version,
		partialPath(u.Layout.StateDir, target.BinarySHA256), target.BinarySHA256)
}

func (u *Updater) logger() *slog.Logger {
	if u.Logger != nil {
		return u.Logger
	}
	return slog.Default()
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func runCommand(ctx context.Context, binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var stderr bytes.Buffer
	command := exec.CommandContext(ctx, binary, args...)
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 512 {
			detail = detail[:512]
		}
		return "", fmt.Errorf("%w: %s", err, detail)
	}
	return string(output), nil
}

func digest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}
