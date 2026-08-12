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

type Result struct {
	Code       string
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
}

func New(config Config) (*Provider, error) {
	if config.Program == "" {
		return nil, errors.New("ChangeIP program is required")
	}
	info, err := os.Stat(config.Program)
	if err != nil {
		return nil, fmt.Errorf("stat ChangeIP program: %w", err)
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

func (p *Provider) Run(ctx context.Context) Result {
	startedAt := p.now().UTC()
	runContext, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()

	process := exec.Command(p.config.Program, p.config.Args...)
	process.Stdin = nil
	process.Stdout = io.Discard
	process.Stderr = io.Discard
	configureProcess(process)
	if err := process.Start(); err != nil {
		return Result{Code: CodeStartFailed, ExitCode: -1, StartedAt: startedAt, FinishedAt: p.now().UTC()}
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
		return Result{Code: code, ExitCode: exitCode(process), StartedAt: startedAt, FinishedAt: p.now().UTC()}
	}
}

func resultFromWait(waitError error, process *exec.Cmd, startedAt, finishedAt time.Time) Result {
	if waitError == nil {
		return Result{Code: CodeCompleted, ExitCode: 0, StartedAt: startedAt, FinishedAt: finishedAt}
	}
	return Result{Code: CodeExitedNonZero, ExitCode: exitCode(process), StartedAt: startedAt, FinishedAt: finishedAt}
}

func exitCode(process *exec.Cmd) int {
	if process.ProcessState == nil {
		return -1
	}
	return process.ProcessState.ExitCode()
}
