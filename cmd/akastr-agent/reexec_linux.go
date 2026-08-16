//go:build linux

package main

import (
	"os"
	"syscall"
)

func reexecAgent(binary, configPath string) error {
	return syscall.Exec(
		binary,
		[]string{binary, "run", "--config", configPath},
		os.Environ(),
	)
}
