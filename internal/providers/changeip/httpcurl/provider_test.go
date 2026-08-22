package httpcurl

import (
	"errors"
	"testing"

	changeprovider "github.com/akastrmix/akastr-agent/internal/providers/changeip"
)

func TestClassifyRequiresExactHTTP200(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		exitCode int
		status   string
		state    changeprovider.TriggerState
		code     string
	}{
		{name: "200", status: "200", state: changeprovider.TriggerConfirmed, code: CodeCompleted},
		{name: "200 with incomplete transfer", err: errors.New("exit 18"), exitCode: 18, status: "200", state: changeprovider.TriggerUnknown, code: CodeTriggerOutcomeUnknown},
		{name: "redirect", status: "302", state: changeprovider.TriggerFailed, code: CodeHTTPStatusNot200},
		{name: "server error", err: errors.New("exit 22"), exitCode: 22, status: "502", state: changeprovider.TriggerFailed, code: CodeHTTPStatusNot200},
		{name: "dns", err: errors.New("exit 6"), exitCode: 6, status: "000", state: changeprovider.TriggerFailed, code: CodeRequestFailed},
		{name: "response lost", err: errors.New("exit 56"), exitCode: 56, status: "000", state: changeprovider.TriggerUnknown, code: CodeTriggerOutcomeUnknown},
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

func TestConfigFileAcceptsOnlyInstallerOrRevisionManagedPaths(t *testing.T) {
	for _, accepted := range []string{
		"/etc/akastr-agent/changeip-curl.conf",
		"/var/lib/akastr-agent/configurations/2/changeip-curl.conf",
	} {
		if !validConfigFile(accepted) {
			t.Fatalf("managed path rejected: %s", accepted)
		}
	}
	for _, rejected := range []string{
		"/var/lib/akastr-agent/configurations/0/changeip-curl.conf",
		"/var/lib/akastr-agent/configurations/2/../changeip-curl.conf",
		"/tmp/changeip-curl.conf",
	} {
		if validConfigFile(rejected) {
			t.Fatalf("unmanaged path accepted: %s", rejected)
		}
	}
}
