// Package diagnostics runs a script that an operator sends through Cloud in a
// throwaway systemd sandbox that can read the node but change nothing: the
// file system is read-only apart from a private /tmp, the script runs as a
// transient user whose only privilege is reading every file, it cannot reach
// local ports (the 3x-ui Xray API needs no password), and the unit with every
// process it started is gone when the script ends. The answer is sent once and
// never stored; logs name the script, never its output (Cloud ADR 0102).
//
// Units outside this process: one per running script, named unitPrefix plus
// the request ID. The session ending or the time limit stops it; a process
// replaced by an update or a crash leaves it to the Sweeper of the next
// process, and RuntimeMaxSec ends it even if no Agent runs again.
package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const (
	Name        = "diagnostics"
	RequestType = "diagnostics.run"
	ResultType  = "diagnostics.result"

	maxScript = 64 << 10
	maxStream = 256 << 10
	timeLimit = 5 * time.Minute
	// The unit outlives a lost Agent by at most this much more.
	runtimeMargin = 30 * time.Second

	unitPrefix     = "akastr-diagnostics-"
	systemdRun     = "/usr/bin/systemd-run"
	systemctl      = "/usr/bin/systemctl"
	scriptVariable = "AKASTR_DIAGNOSTICS_SCRIPT"
	// IPAddressDeny needs the unified cgroup hierarchy; elsewhere systemd only
	// warns and the script could reach the Xray API.
	unifiedCgroup = "/sys/fs/cgroup/cgroup.controllers"
)

type Config struct{}

func ParseConfig(raw json.RawMessage) (Config, error) {
	return protocol.DecodeStrict[Config](raw, Name)
}

type Request struct {
	RequestID string `json:"request_id"`
	Script    string `json:"script"`
}

func DecodeRequest(raw json.RawMessage) (Request, error) {
	request, err := protocol.DecodeStrict[Request](raw, RequestType, "request_id", "script")
	if err != nil {
		return Request{}, err
	}
	if !protocol.ValidUUID(request.RequestID) || request.Script == "" ||
		len(request.Script) > maxScript || strings.ContainsRune(request.Script, 0) {
		return Request{}, errors.New("diagnostics request is invalid")
	}
	return request, nil
}

type Result struct {
	RequestID       string `json:"request_id"`
	Code            string `json:"code"`
	ExitCode        *int   `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	DurationMS      int64  `json:"duration_ms"`
}

// Runner answers diagnostics.run, one script at a time.
type Runner struct {
	swept     <-chan struct{}
	timeLimit time.Duration

	mu      sync.Mutex
	running bool
}

var _ module.Queries = (*Runner)(nil)

// New returns a runner that starts scripts once sweeper has stopped what an
// earlier process left running.
func New(sweeper *Sweeper) *Runner {
	return &Runner{swept: sweeper.done, timeLimit: timeLimit}
}

func (r *Runner) Types() (string, string) { return RequestType, ResultType }

func (r *Runner) Validate(body json.RawMessage) error {
	_, err := DecodeRequest(body)
	return err
}

// Answer runs the script and returns its result, or nil when the session
// ended first: the unit is stopped and nobody is left to answer.
func (r *Runner) Answer(ctx context.Context, body json.RawMessage) any {
	request, err := DecodeRequest(body)
	if err != nil {
		return nil
	}
	result, answered := r.run(ctx, request)
	if !answered {
		return nil
	}
	return fit(result)
}

func (r *Runner) run(ctx context.Context, request Request) (Result, bool) {
	result := Result{RequestID: request.RequestID}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		result.Code = "busy"
		return result, true
	}
	r.running = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()
	if err := r.ready(); err != nil {
		slog.Warn("diagnostics sandbox unavailable", "request_id", request.RequestID, "error", err.Error())
		result.Code = "sandbox_unavailable"
		return result, true
	}

	started := time.Now()
	unit := unitPrefix + request.RequestID + ".service"
	slog.Info("diagnostics script started", "request_id", request.RequestID, "script", request.Script)
	stdout, stderr := &capped{limit: maxStream}, &capped{limit: maxStream}
	command := exec.Command(systemdRun, sandboxArguments(unit, request.Script, r.timeLimit)...)
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Start(); err != nil {
		slog.Warn("diagnostics script did not start", "request_id", request.RequestID, "error", err.Error())
		result.Code = "start_failed"
		return result, true
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	timer := time.NewTimer(r.timeLimit)
	defer timer.Stop()
	var waitErr error
	result.Code = "completed"
	select {
	case waitErr = <-done:
	case <-timer.C:
		result.Code = "timed_out"
		stop(command, unit, done)
	case <-ctx.Done():
		stop(command, unit, done)
		slog.Info("diagnostics script stopped with its session", "request_id", request.RequestID)
		return Result{}, false
	}

	result.DurationMS = time.Since(started).Milliseconds()
	result.Stdout, result.StdoutTruncated = stdout.text(), stdout.truncated
	result.Stderr, result.StderrTruncated = stderr.text(), stderr.truncated
	if result.Code == "completed" {
		exit := 0
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) {
			exit = exitError.ExitCode()
		}
		if exit >= 0 {
			result.ExitCode = &exit
		}
	}
	slog.Info("diagnostics script finished", "request_id", request.RequestID, "code", result.Code,
		"exit_code", exitText(result.ExitCode), "duration_ms", result.DurationMS,
		"stdout_bytes", len(stdout.data), "stderr_bytes", len(stderr.data),
		"truncated", stdout.truncated || stderr.truncated)
	return result, true
}

func (r *Runner) ready() error {
	select {
	case <-r.swept:
	default:
		return errors.New("units of an earlier process are not stopped yet")
	}
	for _, path := range []string{systemdRun, systemctl, unifiedCgroup} {
		if _, err := os.Stat(path); err != nil {
			return err
		}
	}
	return nil
}

// sandboxArguments is the whole sandbox. The script reaches bash through the
// environment: systemd expands variables in command arguments, never in
// environment values.
func sandboxArguments(unit, script string, limit time.Duration) []string {
	runtimeMax := strconv.Itoa(int((limit + runtimeMargin) / time.Second))
	return []string{
		"--quiet", "--pipe", "--wait", "--collect", "--service-type=exec",
		"--unit=" + unit, "--description=akastr-diagnostics",
		"--property=DynamicUser=yes",
		"--property=NoNewPrivileges=yes",
		"--property=AmbientCapabilities=CAP_DAC_READ_SEARCH",
		"--property=CapabilityBoundingSet=CAP_DAC_READ_SEARCH",
		"--property=ProtectSystem=strict",
		"--property=ProtectHome=read-only",
		"--property=PrivateTmp=yes",
		"--property=PrivateDevices=yes",
		"--property=IPAddressDeny=localhost",
		// The systemd-resolved stub, where a node resolves names.
		"--property=IPAddressAllow=127.0.0.53",
		"--property=RuntimeMaxSec=" + runtimeMax,
		"--property=MemoryMax=256M",
		"--property=CPUQuota=50%",
		"--property=TasksMax=64",
		"--setenv=" + scriptVariable + "=" + script,
		"/bin/bash", "-c", `eval "$` + scriptVariable + `"`,
	}
}

// stop ends the unit and every process in it; systemd-run then returns.
// Killing systemd-run alone would leave the unit running.
func stop(command *exec.Cmd, unit string, done <-chan error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, systemctl, "stop", unit).Run(); err != nil {
		slog.Warn("diagnostics unit did not stop; its time limit ends it", "unit", unit, "error", err.Error())
		_ = command.Process.Kill()
	}
	<-done
}

// Sweeper stops the units an earlier Agent process left running. It runs
// whether or not the module is enabled, so switching the module off ends them
// as well.
type Sweeper struct{ done chan struct{} }

var _ module.Worker = (*Sweeper)(nil)

func NewSweeper() *Sweeper { return &Sweeper{done: make(chan struct{})} }

func (s *Sweeper) Run(ctx context.Context) error {
	for {
		stopContext, cancel := context.WithTimeout(ctx, time.Minute)
		err := exec.CommandContext(stopContext, systemctl, "stop", unitPrefix+"*.service").Run()
		cancel()
		if err == nil {
			close(s.done)
			<-ctx.Done()
			return ctx.Err()
		}
		if ctx.Err() == nil {
			slog.Warn("diagnostics units of an earlier process remain; retrying", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Minute):
		}
	}
}

// capped keeps the first limit bytes written and drains the rest, so a script
// printing without end never blocks.
type capped struct {
	limit     int
	data      []byte
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	written := len(p)
	if room := c.limit - len(c.data); len(p) > room {
		c.truncated = true
		p = p[:max(room, 0)]
	}
	c.data = append(c.data, p...)
	return written, nil
}

func (c *capped) text() string { return strings.ToValidUTF8(string(c.data), "�") }

// fit shortens the longer output until the answer fits one protocol message;
// escaping can make the encoded text several times its size.
func fit(result Result) Result {
	for {
		encoded, err := protocol.Encode(ResultType, result)
		if err == nil && len(encoded) <= protocol.MaxMessage {
			return result
		}
		if len(result.Stdout) >= len(result.Stderr) {
			result.Stdout, result.StdoutTruncated = half(result.Stdout), true
		} else {
			result.Stderr, result.StderrTruncated = half(result.Stderr), true
		}
	}
}

func half(text string) string {
	end := len(text) / 2
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

func exitText(code *int) string {
	if code == nil {
		return "none"
	}
	return strconv.Itoa(*code)
}
