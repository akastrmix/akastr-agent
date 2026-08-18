package script

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewRejectsMissingRuntimeDependency(t *testing.T) {
	directory := t.TempDir()
	scriptPath := filepath.Join(directory, "ip.sh")
	script := []byte("#!/bin/bash\nexit 0\n")
	if err := os.WriteFile(scriptPath, script, 0o700); err != nil {
		t.Fatal(err)
	}
	profilesPath := filepath.Join(directory, "profiles.json")
	if err := os.WriteFile(profilesPath, []byte(`{
  "schema_version": 1,
  "profiles": {"target": {"username": "user", "password": "secret"}}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(script)
	t.Setenv("PATH", directory)

	_, err := New(Config{
		ScriptPath: scriptPath, ProfilesFile: profilesPath, Timeout: time.Minute,
		ScriptVersion: "test", ExpectedSHA256Hex: hex.EncodeToString(digest[:]),
	})
	if err == nil || !strings.Contains(err.Error(), "required command") {
		t.Fatalf("New() error = %v, want missing runtime dependency", err)
	}
}

func TestReportURLIsRecognizedInNonZeroScriptOutput(t *testing.T) {
	output := &limitedBuffer{limit: 1024}
	_, _ = output.Write([]byte("completed\nhttps://Report.Check.Place/abc-123\n"))
	got, code := interpretScriptOutput(output, errors.New("exit status 1"))
	if got != "https://Report.Check.Place/abc-123" || code != "" {
		t.Fatalf("interpretScriptOutput() = %q, %q", got, code)
	}
}

func TestInterpretScriptOutputFailures(t *testing.T) {
	for _, test := range []struct {
		name         string
		output       string
		overflow     bool
		processError error
		wantCode     string
	}{
		{name: "non-zero without report", processError: errors.New("exit status 1"), wantCode: "script_failed"},
		{name: "zero without report", wantCode: "report_url_missing"},
		{name: "overflow takes precedence", output: "https://Report.Check.Place/complete", overflow: true, wantCode: "script_output_too_large"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := &limitedBuffer{limit: 1024}
			_, _ = output.Write([]byte(test.output))
			output.overflow = test.overflow
			_, code := interpretScriptOutput(output, test.processError)
			if code != test.wantCode {
				t.Fatalf("interpretScriptOutput() code = %q, want %q", code, test.wantCode)
			}
		})
	}
}

func TestProfileIDsAreSortedWithoutCredentials(t *testing.T) {
	profilesPath := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(profilesPath, []byte(`{
  "schema_version": 1,
  "profiles": {
    "z-secondary": {"username": "second-user", "password": "second-secret"},
    "a-primary": {"username": "first-user", "password": "first-secret"}
  }
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ids, err := ProfileIDs(profilesPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "a-primary" || ids[1] != "z-secondary" {
		t.Fatalf("ProfileIDs() = %#v", ids)
	}
}
