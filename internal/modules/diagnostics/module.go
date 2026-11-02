// Package diagnostics runs a script that an operator sends through Cloud in a
// throwaway systemd sandbox that can read the node but change nothing: the
// file system is read-only and temporary files go to memory, the script
// runs as a transient user whose only privilege is reading every file, it can
// write neither to the journal nor to local ports (the 3x-ui Xray API needs no
// password), and the unit with every process it started is gone when the
// script ends. The answer is sent once and never stored; logs name the
// script, never its output (Cloud ADR 0102).
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
	"net"
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
)

// localBlock keeps the script off every local port. Where cgroup BPF is not
// available, as in some containers, systemd ignores it without failing, so
// each run first proves it holds (see probeAndRun).
var localBlock = []string{
	"--property=IPAddressDeny=localhost",
	// The systemd-resolved stub, where a node resolves names.
	"--property=IPAddressAllow=127.0.0.53",
}

// probeAndRun connects to a port the Agent listens on before running the
// script. A blocked connection is dropped, not refused, so it waits a second;
// one that succeeds means the block does not hold and the script never runs.
const probeAndRun = `if timeout 1 bash -c 'exec 3<>/dev/tcp/127.0.0.1/$0' "$1" 2>/dev/null; then exit 125; fi; set --; eval "$` + scriptVariable + `"`

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
	swept      <-chan struct{}
	timeLimit  time.Duration
	localBlock []string

	mu      sync.Mutex
	running bool
}

var _ module.Queries = (*Runner)(nil)

// New returns a runner that starts scripts once sweeper has stopped what an
// earlier process left running.
func New(sweeper *Sweeper) *Runner {
	return &Runner{swept: sweeper.done, timeLimit: timeLimit, localBlock: localBlock}
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
	select {
	case <-r.swept:
	default:
		slog.Warn("diagnostics units of an earlier process are not stopped yet", "request_id", request.RequestID)
		result.Code = "sandbox_unavailable"
		return result, true
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		slog.Warn("diagnostics probe did not listen", "request_id", request.RequestID, "error", err.Error())
		result.Code = "start_failed"
		return result, true
	}
	defer probe.Close()
	probePort := probe.Addr().(*net.TCPAddr).Port

	started := time.Now()
	unit := unitPrefix + request.RequestID + ".service"
	slog.Info("diagnostics script started", "request_id", request.RequestID, "script", request.Script)
	stdout, stderr := &capped{limit: maxStream}, &capped{limit: maxStream}
	command := exec.Command(systemdRun, sandboxArguments(unit, request.Script, r.timeLimit, r.localBlock, probePort)...)
	command.Stdout, command.Stderr = stdout, stderr
	// systemd-run returning ends the wait for output even if the unit is not gone.
	command.WaitDelay = 10 * time.Second
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

	if reached(probe) {
		slog.Warn("diagnostics sandbox reached a local port; script not run", "request_id", request.RequestID)
		return Result{RequestID: request.RequestID, Code: "sandbox_unavailable"}, true
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

// reached reports whether the sandbox connected to the probe; the connection
// waits in the backlog, so a short deadline finds it.
func reached(probe net.Listener) bool {
	_ = probe.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond))
	connection, err := probe.Accept()
	if err != nil {
		return false
	}
	connection.Close()
	return true
}

// sandboxArguments is the whole sandbox. The script reaches bash through the
// environment: systemd expands variables in command arguments, never in
// environment values.
func sandboxArguments(unit, script string, limit time.Duration, block []string, probePort int) []string {
	runtimeMax := strconv.Itoa(int((limit + runtimeMargin) / time.Second))
	arguments := []string{
		"--quiet", "--pipe", "--wait", "--collect", "--service-type=exec",
		"--unit=" + unit, "--description=akastr-diagnostics",
		"--property=DynamicUser=yes",
		"--property=NoNewPrivileges=yes",
		"--property=AmbientCapabilities=CAP_DAC_READ_SEARCH",
		"--property=CapabilityBoundingSet=CAP_DAC_READ_SEARCH",
		"--property=ProtectSystem=strict",
		"--property=ProtectHome=read-only",
		// DynamicUser brings private /tmp and /var/tmp, and before systemd 257
		// they are directories on disk that win over a tmpfs at the same path.
		// Closing them wins on every version; temporary files go to /dev/shm,
		// in memory and removed with the user's other IPC (RemoveIPC).
		"--property=InaccessiblePaths=/tmp /var/tmp",
		"--setenv=TMPDIR=/dev/shm",
		// /dev/shm is the host's: what the script writes there is its own alone.
		"--property=UMask=0077",
		"--property=PrivateDevices=yes",
		// Reading the journal stays possible; writing to it would outlive the unit.
		"--property=InaccessiblePaths=-/run/systemd/journal",
		"--property=RuntimeMaxSec=" + runtimeMax,
		// Nothing a read-only script does needs a graceful stop.
		"--property=TimeoutStopSec=5s",
		"--property=MemoryMax=256M",
		"--property=CPUQuota=50%",
		"--property=TasksMax=64",
		"--setenv=" + scriptVariable + "=" + script,
	}
	arguments = append(arguments, block...)
	return append(arguments, "/bin/bash", "-c", probeAndRun, "akastr-diagnostics", strconv.Itoa(probePort))
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
