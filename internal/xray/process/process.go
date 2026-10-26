// Package process finds the one Xray process 3x-ui runs and reads what it
// was started with: its configuration file and whether it splices.
package process

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Xray is the process, as seen under a proc directory.
type Xray struct {
	dir string
	// Cwd is its working directory, against which its relative paths resolve.
	Cwd string
}

// Find finds the one Xray process 3x-ui runs.
func Find(proc string) (Xray, error) {
	dirs, err := os.ReadDir(proc)
	if err != nil {
		return Xray{}, err
	}
	var pids []string
	for _, d := range dirs {
		if _, err := strconv.Atoi(d.Name()); err != nil {
			continue
		}
		// 3x-ui names its binary xray-<os>-<arch>.
		if exe, err := os.Readlink(filepath.Join(proc, d.Name(), "exe")); err == nil && strings.HasPrefix(filepath.Base(exe), "xray-linux-") {
			pids = append(pids, d.Name())
		}
	}
	if len(pids) != 1 {
		return Xray{}, fmt.Errorf("found %d Xray processes started by 3x-ui", len(pids))
	}
	dir := filepath.Join(proc, pids[0])
	cwd, err := os.Readlink(filepath.Join(dir, "cwd"))
	if err != nil {
		return Xray{}, err
	}
	return Xray{dir: dir, Cwd: cwd}, nil
}

// Config is the path of the configuration file it runs.
func (x Xray) Config() (string, error) {
	cmdline, err := os.ReadFile(filepath.Join(x.dir, "cmdline"))
	if err != nil {
		return "", err
	}
	config := configArg(strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00"))
	if config == "" {
		return "", errors.New("the Xray command line names no configuration file")
	}
	return x.Resolve(config), nil
}

func configArg(args []string) string {
	for i, arg := range args {
		for _, flag := range []string{"-c", "-config", "--config"} {
			if arg == flag && i+1 < len(args) {
				return args[i+1]
			}
			if value, ok := strings.CutPrefix(arg, flag+"="); ok {
				return value
			}
		}
	}
	return ""
}

// Resolve reads a path the way the process would.
func (x Xray) Resolve(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(x.Cwd, path)
}

// Spliced says whether Xray hands the downlink of a Vision connection to the
// kernel, as the environment it started with decides. It then adds that
// connection's bytes to its user statistics only when the connection ends.
// Xray reads the flag by its own name, then by its environment name; set to
// anything but auto or enable, even empty, it is off. Only those two entries
// are read out of the environment, which may hold secrets.
func (x Xray) Spliced() (bool, error) {
	environ, err := os.ReadFile(filepath.Join(x.dir, "environ"))
	if err != nil {
		return false, err
	}
	for _, key := range []string{"xray.buf.splice", "XRAY_BUF_SPLICE"} {
		for _, entry := range bytes.Split(environ, []byte{0}) {
			if value, ok := bytes.CutPrefix(entry, []byte(key+"=")); ok {
				return string(value) == "auto" || string(value) == "enable", nil
			}
		}
	}
	return true, nil
}
