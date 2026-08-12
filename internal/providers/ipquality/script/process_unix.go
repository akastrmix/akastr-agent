//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package script

import (
	"os/exec"
	"syscall"
)

func configureProcess(process *exec.Cmd) {
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcess(process *exec.Cmd) {
	if process.Process != nil {
		_ = syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
	}
}
