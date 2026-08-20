//go:build linux

package main

import (
	"os"
	"strings"
	"syscall"

	"github.com/akastrmix/akastr-agent/internal/autoupdate"
)

func reexecAgent(binary, configPath, version string) error {
	environment := make([]string, 0, len(os.Environ())+1)
	prefix := autoupdate.TrialVersionEnvironment + "="
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, prefix) {
			environment = append(environment, entry)
		}
	}
	environment = append(environment, prefix+version)
	return syscall.Exec(
		binary,
		[]string{binary, "run", "--config", configPath},
		environment,
	)
}
