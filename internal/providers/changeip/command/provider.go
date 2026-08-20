package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"

	changeprovider "github.com/akastrmix/akastr-agent/internal/providers/changeip"
)

const (
	CodeCompleted     = "completed"
	CodeStartFailed   = "start_failed"
	CodeExitedNonZero = "exited_nonzero"
	CodeTimedOut      = "timed_out"
	CodeCancelled     = "cancelled"
)

type Config struct {
	Program string
	Args    []string
	Timeout time.Duration
}

type Provider struct {
	config Config
	now    func() time.Time
}

func New(config Config) (*Provider, error) {
	if config.Program == "" {
		return nil, errors.New("ChangeIP program is required")
	}
	info, err := os.Lstat(config.Program)
	if err != nil {
		return nil, fmt.Errorf("stat ChangeIP program: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("ChangeIP program must not be a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("ChangeIP program must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("ChangeIP program must be executable")
	}
	if config.Timeout <= 0 || config.Timeout > 5*time.Minute {
		return nil, errors.New("ChangeIP timeout must be positive and no longer than 5 minutes")
	}
	return &Provider{config: config, now: time.Now}, nil
}

func (p *Provider) Run(ctx context.Context) changeprovider.Result {
	startedAt := p.now().UTC()
	runContext, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()

	process := exec.Command(p.config.Program, p.config.Args...)
	process.Stdin = nil
	process.Stdout = io.Discard
	process.Stderr = io.Discard
	configureProcess(process)
	if err := process.Start(); err != nil {
		return changeprovider.Result{State: changeprovider.TriggerFailed, Code: CodeStartFailed, ExitCode: -1, StartedAt: startedAt, FinishedAt: p.now().UTC()}
	}

	done := make(chan error, 1)
	go func() {
		done <- process.Wait()
	}()

	select {
	case waitError := <-done:
		return resultFromWait(waitError, process, startedAt, p.now().UTC())
	case <-runContext.Done():
		terminateProcess(process)
		<-done
		code := CodeTimedOut
		if ctx.Err() != nil {
			code = CodeCancelled
		}
		return changeprovider.Result{State: changeprovider.TriggerUnknown, Code: code, ExitCode: exitCode(process), StartedAt: startedAt, FinishedAt: p.now().UTC()}
	}
}

func resultFromWait(waitError error, process *exec.Cmd, startedAt, finishedAt time.Time) changeprovider.Result {
	if waitError == nil {
		return changeprovider.Result{State: changeprovider.TriggerConfirmed, Code: CodeCompleted, ExitCode: 0, StartedAt: startedAt, FinishedAt: finishedAt}
	}
	return changeprovider.Result{State: changeprovider.TriggerFailed, Code: CodeExitedNonZero, ExitCode: exitCode(process), StartedAt: startedAt, FinishedAt: finishedAt}
}

func exitCode(process *exec.Cmd) int {
	if process.ProcessState == nil {
		return -1
	}
	return process.ProcessState.ExitCode()
}
