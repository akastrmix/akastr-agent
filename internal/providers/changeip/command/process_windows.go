//go:build windows

package command

import "os/exec"

func configureProcess(_ *exec.Cmd) {}

func terminateProcess(process *exec.Cmd) {
	if process.Process != nil {
		_ = process.Process.Kill()
	}
}
