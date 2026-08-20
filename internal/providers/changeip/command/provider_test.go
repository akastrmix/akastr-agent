package command

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestProviderCompletesWithoutShell(t *testing.T) {
	provider := newHelperProvider(t, "success", time.Second)
	result := provider.Run(context.Background())
	if result.Code != CodeCompleted || result.ExitCode != 0 {
		t.Fatalf("Run() = %#v", result)
	}
}

func TestProviderReportsNonZeroExit(t *testing.T) {
	provider := newHelperProvider(t, "failure", time.Second)
	result := provider.Run(context.Background())
	if result.Code != CodeExitedNonZero || result.ExitCode != 23 {
		t.Fatalf("Run() = %#v", result)
	}
}

func TestProviderTimesOutAndTerminates(t *testing.T) {
	provider := newHelperProvider(t, "sleep", 50*time.Millisecond)
	result := provider.Run(context.Background())
	if result.Code != CodeTimedOut {
		t.Fatalf("Run() = %#v", result)
	}
}

func TestProviderHonorsParentCancellation(t *testing.T) {
	provider := newHelperProvider(t, "sleep", time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := provider.Run(ctx)
	if result.Code != CodeCancelled {
		t.Fatalf("Run() = %#v", result)
	}
}

func TestProviderRejectsProgramSymlink(t *testing.T) {
	directory := t.TempDir()
	target := directory + string(os.PathSeparator) + "changeip-target"
	link := directory + string(os.PathSeparator) + "changeip"
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	_, err := New(Config{Program: link, Timeout: time.Second})
	if err == nil || err.Error() != "ChangeIP program must not be a symbolic link" {
		t.Fatalf("New() error = %v, want symbolic link rejection", err)
	}
}

func TestCommandHelperProcess(t *testing.T) {
	if os.Getenv("AKASTR_AGENT_HELPER_PROCESS") != "1" {
		return
	}
	switch os.Getenv("AKASTR_AGENT_HELPER_MODE") {
	case "success":
		os.Exit(0)
	case "failure":
		os.Exit(23)
	case "sleep":
		time.Sleep(5 * time.Second)
		os.Exit(0)
	default:
		os.Exit(99)
	}
}

func newHelperProvider(t *testing.T, mode string, timeout time.Duration) *Provider {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AKASTR_AGENT_HELPER_PROCESS", "1")
	t.Setenv("AKASTR_AGENT_HELPER_MODE", mode)
	provider, err := New(Config{
		Program: executable,
		Args:    []string{"-test.run=TestCommandHelperProcess"},
		Timeout: timeout,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return provider
}
