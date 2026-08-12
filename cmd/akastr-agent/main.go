package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/akastrmix/akastr-agent/internal/app"
	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/identity"
	transportws "github.com/akastrmix/akastr-agent/internal/transport/ws"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "akastr-agent:", err)
		os.Exit(1)
	}
}

func run(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("expected one of: run, enroll, check-config, capabilities, version")
	}
	switch arguments[0] {
	case "version":
		if len(arguments) != 1 {
			return errors.New("version accepts no arguments")
		}
		_, err := fmt.Fprintln(output, version)
		return err
	case "run", "enroll", "check-config", "capabilities":
		flags := flag.NewFlagSet(arguments[0], flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		configPath := flags.String("config", "/etc/akastr-agent/config.json", "configuration file")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		model, err := app.Load(*configPath)
		if err != nil {
			return err
		}
		if arguments[0] == "check-config" {
			if _, err := app.BuildRuntime(model); err != nil {
				return fmt.Errorf("validate runtime dependencies: %w", err)
			}
			_, err := fmt.Fprintln(output, "configuration valid")
			return err
		}
		if arguments[0] == "enroll" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			created, err := identity.Enroll(ctx, struct {
				Endpoint        string
				TokenFile       string
				IdentityFile    string
				ExpectedAgentID string
				AgentVersion    string
				Capabilities    []capability.Descriptor
				HTTPClient      *http.Client
			}{
				Endpoint:        model.Config.Control.Endpoint,
				TokenFile:       model.Config.Control.EnrollmentTokenFile,
				IdentityFile:    model.Config.Control.CredentialFile,
				ExpectedAgentID: model.Config.Node.ID,
				AgentVersion:    version,
				Capabilities:    model.Capabilities.List(),
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(output, "enrolled agent %s\n", created.AgentID)
			return err
		}
		if arguments[0] == "run" {
			credentials, err := identity.Load(model.Config.Control.CredentialFile)
			if err != nil {
				return err
			}
			if credentials.AgentID != model.Config.Node.ID {
				return errors.New("configured node ID does not match enrolled identity")
			}
			runtime, err := app.BuildRuntime(model)
			if err != nil {
				return err
			}
			logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
			client, err := transportws.New(struct {
				Endpoint     string
				Identity     identity.Identity
				Version      string
				Capabilities []capability.Descriptor
				Executor     transportws.Executor
				Observations transportws.ObservationSource
				Logger       *slog.Logger
			}{
				Endpoint: model.Config.Control.Endpoint, Identity: credentials,
				Version: version, Capabilities: model.Capabilities.List(),
				Executor: runtime, Observations: runtime.IPMonitor(), Logger: logger,
			})
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			err = client.Run(ctx)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(model.Capabilities.List())
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}
