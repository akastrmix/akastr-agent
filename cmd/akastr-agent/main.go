package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/akastrmix/akastr-agent/internal/app"
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
		return errors.New("expected one of: check-config, capabilities, version")
	}
	switch arguments[0] {
	case "version":
		if len(arguments) != 1 {
			return errors.New("version accepts no arguments")
		}
		_, err := fmt.Fprintln(output, version)
		return err
	case "check-config", "capabilities":
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
			_, err := fmt.Fprintln(output, "configuration valid")
			return err
		}
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(model.Capabilities.List())
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}
