package httpcurl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/modules/changeip"
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
	Program     string
	URL         string
	BearerToken string
	Timeout     time.Duration
}

type Provider struct {
	config Config
	now    func() time.Time
}

func New(config Config) (*Provider, error) {
	if config.Program != "/usr/bin/curl" || config.URL == "" || config.BearerToken == "" ||
		strings.ContainsAny(config.URL+config.BearerToken, "\r\n") || strings.ContainsAny(config.BearerToken, "\"\\") {
		return nil, errors.New("HTTP ChangeIP provider configuration is invalid")
	}
	info, err := os.Stat(config.Program)
	if err != nil {
		return nil, fmt.Errorf("stat curl: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("curl must be an executable regular file")
	}
	if config.Timeout <= 0 || config.Timeout > 5*time.Minute {
		return nil, errors.New("HTTP ChangeIP timeout must be positive and no longer than 5 minutes")
	}
	return &Provider{config: config, now: time.Now}, nil
}

func (p *Provider) Run(ctx context.Context) changeip.Result {
	startedAt := p.now().UTC()
	runContext, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	// The URL and bearer token reach curl through its stdin config, never argv or disk.
	process := exec.Command(
		p.config.Program,
		"--config", "-",
		"--output", os.DevNull,
		"--write-out", "%{http_code}",
	)
	process.Stdin = strings.NewReader(fmt.Sprintf(
		"url = \"%s\"\nrequest = \"POST\"\nheader = \"Authorization: Bearer %s\"\nfail\nsilent\nshow-error\n",
		strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(p.config.URL), p.config.BearerToken,
	))
	var output bytes.Buffer
	process.Stdout = &output
	process.Stderr = nil
	if err := process.Start(); err != nil {
		return p.result(changeip.TriggerFailed, CodeRequestFailed, -1, startedAt)
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
		return p.result(changeip.TriggerUnknown, code, process.ProcessState.ExitCode(), startedAt)
	}
}

func classify(waitError error, exitCode int, status string) (changeip.TriggerState, string) {
	if status == "200" {
		if waitError == nil {
			return changeip.TriggerConfirmed, CodeCompleted
		}
		return changeip.TriggerUnknown, CodeTriggerOutcomeUnknown
	}
	if len(status) == 3 && status != "000" && status[0] >= '1' && status[0] <= '5' {
		return changeip.TriggerFailed, CodeHTTPStatusNot200
	}
	if exitCode == 6 || exitCode == 7 || exitCode == 35 {
		return changeip.TriggerFailed, CodeRequestFailed
	}
	return changeip.TriggerUnknown, CodeTriggerOutcomeUnknown
}

func (p *Provider) result(state changeip.TriggerState, code string, exitCode int, startedAt time.Time) changeip.Result {
	return changeip.Result{
		State: state, Code: code, ExitCode: exitCode,
		StartedAt: startedAt, FinishedAt: p.now().UTC(),
	}
}
