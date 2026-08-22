//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/akastrmix/akastr-agent/internal/autoupdate"
)

func reexecAgent(binary, configPath, version string, revision int64) error {
	environment := make([]string, 0, len(os.Environ())+2)
	prefix := autoupdate.TrialVersionEnvironment + "="
	revisionPrefix := autoupdate.TrialRevisionEnvironment + "="
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, prefix) && !strings.HasPrefix(entry, revisionPrefix) {
			environment = append(environment, entry)
		}
	}
	environment = append(environment, prefix+version)
	environment = append(environment, revisionPrefix+strconv.FormatInt(revision, 10))
	return syscall.Exec(
		binary,
		[]string{binary, "run", "--config", configPath},
		environment,
	)
}
