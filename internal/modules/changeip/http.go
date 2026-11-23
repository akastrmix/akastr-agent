package changeip

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"
)

const (
	CodeHTTPStatusNot200      = "http_status_not_200"
	CodeRequestFailed         = "request_failed"
	CodeTriggerOutcomeUnknown = "trigger_outcome_unknown"

	// curlProgram sends the HTTP request; the module runs no other program for it.
	curlProgram = "/usr/bin/curl"
	bodyLimit   = 4096
	replyLimit  = 300
)

// httpProvider triggers a change with one HTTP POST through curl; only HTTP
// 200 confirms it.
type httpProvider struct {
	url     string
	token   string
	timeout time.Duration
	now     func() time.Time
}

func newHTTPProvider(url, token string, timeout time.Duration) (*httpProvider, error) {
	if url == "" || token == "" ||
		strings.ContainsAny(url+token, "\r\n") || strings.ContainsAny(token, "\"\\") {
		return nil, errors.New("HTTP ChangeIP provider configuration is invalid")
	}
	info, err := os.Stat(curlProgram)
	if err != nil {
		return nil, fmt.Errorf("stat curl: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("curl must be an executable regular file")
	}
	if timeout <= 0 || timeout > 5*time.Minute {
		return nil, errors.New("HTTP ChangeIP timeout must be positive and no longer than 5 minutes")
	}
	return &httpProvider{url: url, token: token, timeout: timeout, now: time.Now}, nil
}

func (p *httpProvider) Run(ctx context.Context) Result {
	startedAt := p.now().UTC()
	runContext, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	// The URL and bearer token reach curl through its stdin config, never argv or disk.
	// The body goes to stdout and the status to stderr, so neither can be mistaken for the other.
	process := exec.Command(
		curlProgram,
		"--config", "-",
		"--output", "-",
		"--write-out", "%{stderr}%{http_code}",
	)
	process.Stdin = strings.NewReader(fmt.Sprintf(
		"url = \"%s\"\nrequest = \"POST\"\nheader = \"Authorization: Bearer %s\"\nsilent\n",
		strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(p.url), p.token,
	))
	body := &headBuffer{limit: bodyLimit}
	statusOutput := &headBuffer{limit: 64}
	process.Stdout = body
	process.Stderr = statusOutput
	if err := process.Start(); err != nil {
		return p.result(TriggerFailed, CodeRequestFailed, -1, startedAt)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	select {
	case waitError := <-done:
		status := strings.TrimSpace(statusOutput.String())
		exitCode := process.ProcessState.ExitCode()
		state, code := classify(waitError, exitCode, status)
		result := p.result(state, code, exitCode, startedAt)
		if code == CodeHTTPStatusNot200 {
			result.Reply = reply(status, body, p.token)
		}
		return result
	case <-runContext.Done():
		_ = process.Process.Kill()
		<-done
		code := CodeTimedOut
		if ctx.Err() != nil {
			code = CodeCancelled
		}
		return p.result(TriggerUnknown, code, process.ProcessState.ExitCode(), startedAt)
	}
}

func classify(waitError error, exitCode int, status string) (TriggerState, string) {
	if status == "200" {
		if waitError == nil {
			return TriggerConfirmed, CodeCompleted
		}
		return TriggerUnknown, CodeTriggerOutcomeUnknown
	}
	if len(status) == 3 && status != "000" && status[0] >= '1' && status[0] <= '5' {
		return TriggerFailed, CodeHTTPStatusNot200
	}
	if exitCode == 6 || exitCode == 7 || exitCode == 35 {
		return TriggerFailed, CodeRequestFailed
	}
	return TriggerUnknown, CodeTriggerOutcomeUnknown
}

// reply is the refusal as an administrator reads it: the status and the start
// of the body on one line, without the token or control characters.
func reply(status string, head *headBuffer, token string) string {
	data := head.data
	if head.truncated && token != "" {
		// The cut may have split an echoed token; its remains are in the last bytes.
		data = data[:max(0, len(data)-(len(token)-1))]
	}
	body := strings.ToValidUTF8(string(data), "\uFFFD")
	if token != "" {
		body = strings.ReplaceAll(body, token, "<token>")
	}
	body = strings.Join(strings.FieldsFunc(body, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
	if runes := []rune(body); len(runes) > replyLimit {
		body = string(runes[:replyLimit]) + "…"
	}
	if body == "" {
		return "HTTP " + status
	}
	return "HTTP " + status + " " + body
}

// headBuffer keeps the first bytes written to it and drops the rest, so curl
// never blocks on a long body.
type headBuffer struct {
	data      []byte
	limit     int
	truncated bool
}

func (b *headBuffer) Write(p []byte) (int, error) {
	room := b.limit - len(b.data)
	if len(p) > room {
		b.truncated = true
	}
	if room > 0 {
		b.data = append(b.data, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (b *headBuffer) String() string { return string(b.data) }

func (p *httpProvider) result(state TriggerState, code string, exitCode int, startedAt time.Time) Result {
	return Result{
		State: state, Code: code, ExitCode: exitCode,
		StartedAt: startedAt, FinishedAt: p.now().UTC(),
	}
}
