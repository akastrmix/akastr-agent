package httpcurl

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/akastrmix/akastr-agent/internal/modules/changeip"
)

func TestClassifyRequiresExactHTTP200(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		exitCode int
		status   string
		state    changeip.TriggerState
		code     string
	}{
		{name: "200", status: "200", state: changeip.TriggerConfirmed, code: CodeCompleted},
		{name: "200 with incomplete transfer", err: errors.New("exit 18"), exitCode: 18, status: "200", state: changeip.TriggerUnknown, code: CodeTriggerOutcomeUnknown},
		{name: "redirect", status: "302", state: changeip.TriggerFailed, code: CodeHTTPStatusNot200},
		{name: "server error", err: errors.New("exit 22"), exitCode: 22, status: "502", state: changeip.TriggerFailed, code: CodeHTTPStatusNot200},
		{name: "dns", err: errors.New("exit 6"), exitCode: 6, status: "000", state: changeip.TriggerFailed, code: CodeRequestFailed},
		{name: "response lost", err: errors.New("exit 56"), exitCode: 56, status: "000", state: changeip.TriggerUnknown, code: CodeTriggerOutcomeUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, code := classify(test.err, test.exitCode, test.status)
			if state != test.state || code != test.code {
				t.Fatalf("classify() = %q/%q, want %q/%q", state, code, test.state, test.code)
			}
		})
	}
}

// Runs the real curl, so a change to its arguments that breaks telling success
// from refusal, or that lets the token reach the reply, fails here.
func TestRunReportsTheRefusalWithoutTheToken(t *testing.T) {
	if _, err := os.Stat("/usr/bin/curl"); err != nil {
		t.Skip("curl is not installed")
	}
	const token = "secret-token-123"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/ok" {
			_, _ = w.Write([]byte(`{"ip":"changing"}`))
			return
		}
		if r.URL.Path == "/echo-at-limit" {
			// The echoed token starts 5 bytes before the end of what the Agent keeps.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(strings.Repeat(" ", bodyLimit-5) + token))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("{\"error\":\"頻率限制中\",\n\t\"echo\":\"" + token + "\"}" + strings.Repeat("x", 5000)))
	}))
	defer server.Close()

	run := func(path string) changeip.Result {
		provider, err := New(Config{Program: "/usr/bin/curl", URL: server.URL + path, BearerToken: token, Timeout: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return provider.Run(context.Background())
	}
	if result := run("/ok"); result.State != changeip.TriggerConfirmed || result.Code != CodeCompleted || result.Reply != "" {
		t.Fatalf("200: %#v", result)
	}
	refused := run("/refuse")
	wantStart := `HTTP 400 {"error":"頻率限制中", "echo":"<token>"}xxx`
	if refused.State != changeip.TriggerFailed || refused.Code != CodeHTTPStatusNot200 ||
		!strings.HasPrefix(refused.Reply, wantStart) || !strings.HasSuffix(refused.Reply, "x…") ||
		strings.Contains(refused.Reply, token) || utf8.RuneCountInString(refused.Reply) != len("HTTP 400 ")+replyLimit+1 {
		t.Fatalf("400: %#v", refused)
	}
	if cut := run("/echo-at-limit"); cut.Code != CodeHTTPStatusNot200 || cut.Reply != "HTTP 400" {
		t.Fatalf("token split by the cut: %#v", cut)
	}
}
