package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/akastrmix/akastr-agent/internal/config"
	"github.com/akastrmix/akastr-agent/internal/daemon"
	"github.com/akastrmix/akastr-agent/internal/install"
	"github.com/akastrmix/akastr-agent/internal/layout"
)

var version = "dev"

const defaultBootstrapEndpoint = "https://origin.akastrmix.com/internal/agents/bootstrap"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "akastr-agent:", err)
		os.Exit(1)
	}
}

// The commands are a stable contract: an older updater runs `version` and
// `prepare` on a newer candidate, then execs it with `run`.
func run(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("expected one of: run, prepare, install, version")
	}
	flags := flag.NewFlagSet(arguments[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "configuration file")
	agentID := flags.String("agent-id", "", "expected node UUID")
	revision := flags.Int64("revision", 0, "expected configuration revision")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("positional arguments are not accepted")
	}
	switch arguments[0] {
	case "version":
		_, err := fmt.Fprintln(output, version)
		return err
	case "install":
		return runInstall(output)
	case "prepare":
		if *configPath == "" {
			return errors.New("prepare requires --config")
		}
		cfg, err := config.Load(*configPath)
		if err != nil {
			return err
		}
		if (*agentID != "" && cfg.AgentID != *agentID) || (*revision != 0 && cfg.ConfigurationRevision != *revision) {
			return errors.New("configuration does not match the expected node or revision")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		model, err := install.Prepare(ctx, cfg, layout.Default(), nil)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(model.Capabilities.List())
	case "run":
		if *configPath == "" {
			return errors.New("run requires --config")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return daemon.Run(ctx, daemon.Options{
			ConfigPath: *configPath, Version: version, Layout: layout.Default(), Exec: execCandidate,
		})
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

// runInstall takes the node credentials from the environment set by install.sh,
// so the machine token never appears in this process's arguments.
func runInstall(output io.Writer) error {
	agentID, token := os.Getenv("AKASTR_AGENT_ID"), os.Getenv("AKASTR_AGENT_MACHINE_TOKEN")
	endpoint := os.Getenv("AKASTR_AGENT_BOOTSTRAP_ENDPOINT")
	for _, name := range []string{"AKASTR_AGENT_ID", "AKASTR_AGENT_MACHINE_TOKEN", "AKASTR_AGENT_BOOTSTRAP_ENDPOINT"} {
		_ = os.Unsetenv(name)
	}
	if agentID == "" || token == "" {
		return errors.New("AKASTR_AGENT_ID and AKASTR_AGENT_MACHINE_TOKEN are required")
	}
	if endpoint == "" {
		endpoint = defaultBootstrapEndpoint
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	return install.Install(ctx, install.Options{
		AgentID: agentID, MachineToken: token, BootstrapEndpoint: endpoint, Version: version,
		Executable: executable, Layout: layout.Default(),
		UnitFile: "/etc/systemd/system/akastr-agent.service",
		System:   install.RunSystem(output), Output: output,
	})
}

func execCandidate(binary, configPath string) error {
	return syscall.Exec(binary, []string{binary, "run", "--config", configPath}, os.Environ())
}
