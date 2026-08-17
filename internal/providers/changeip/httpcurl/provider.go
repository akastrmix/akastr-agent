package httpcurl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	changeprovider "github.com/akastrmix/akastr-agent/internal/providers/changeip"
)

const (
	CodeCompleted             = "completed"
	CodeHTTPStatusNot200      = "http_status_not_200"
	CodeRequestFailed         = "request_failed"
	CodeTriggerOutcomeUnknown = "trigger_outcome_unknown"
	CodeTimedOut              = "timed_out"
	CodeCancelled             = "cancelled"
)

type Config struct {
	Program    string
	ConfigFile string
	Timeout    time.Duration
}

type Provider struct {
	config Config
	now    func() time.Time
}

func New(config Config) (*Provider, error) {
	if config.Program != "/usr/bin/curl" || config.ConfigFile != "/etc/akastr-agent/changeip-curl.conf" {
		return nil, errors.New("HTTP ChangeIP provider paths are invalid")
	}
	for label, filePath := range map[string]string{"curl": config.Program, "HTTP ChangeIP configuration": config.ConfigFile} {
		info, err := os.Stat(filePath)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", label, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s must be a regular file", label)
		}
		if label == "curl" && runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
			return nil, errors.New("curl must be executable")
		}
	}
	if config.Timeout <= 0 || config.Timeout > 5*time.Minute {
		return nil, errors.New("HTTP ChangeIP timeout must be positive and no longer than 5 minutes")
	}
	return &Provider{config: config, now: time.Now}, nil
}

func (p *Provider) Run(ctx context.Context) changeprovider.Result {
	startedAt := p.now().UTC()
	runContext, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	process := exec.Command(
		p.config.Program,
		"--config", p.config.ConfigFile,
		"--output", os.DevNull,
		"--write-out", "%{http_code}",
	)
	process.Stdin = nil
	var output bytes.Buffer
	process.Stdout = &output
	process.Stderr = nil
	if err := process.Start(); err != nil {
		return p.result(changeprovider.TriggerFailed, CodeRequestFailed, -1, startedAt)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	select {
	case waitError := <-done:
		status := strings.TrimSpace(output.String())
		exitCode := process.ProcessState.ExitCode()
		state, code := classify(waitError, exitCode, status)
		return p.result(state, code, exitCode, startedAt)
	case <-runContext.Done():
		_ = process.Process.Kill()
		<-done
		code := CodeTimedOut
		if ctx.Err() != nil {
			code = CodeCancelled
		}
		return p.result(changeprovider.TriggerUnknown, code, process.ProcessState.ExitCode(), startedAt)
	}
}

func classify(waitError error, exitCode int, status string) (changeprovider.TriggerState, string) {
	if status == "200" {
		if waitError == nil {
			return changeprovider.TriggerConfirmed, CodeCompleted
		}
		return changeprovider.TriggerUnknown, CodeTriggerOutcomeUnknown
	}
	if len(status) == 3 && status != "000" && status[0] >= '1' && status[0] <= '5' {
		return changeprovider.TriggerFailed, CodeHTTPStatusNot200
	}
	if exitCode == 6 || exitCode == 7 || exitCode == 35 {
		return changeprovider.TriggerFailed, CodeRequestFailed
	}
	return changeprovider.TriggerUnknown, CodeTriggerOutcomeUnknown
}

func (p *Provider) result(state changeprovider.TriggerState, code string, exitCode int, startedAt time.Time) changeprovider.Result {
	return changeprovider.Result{
		State: state, Code: code, ExitCode: exitCode,
		StartedAt: startedAt, FinishedAt: p.now().UTC(),
	}
}
