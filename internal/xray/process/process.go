// Package process finds the one Xray process 3x-ui runs and reads what it
// was started with: its binary, configuration file, asset location and
// whether it splices.
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

// Find finds the one Xray process 3x-ui runs. The Agent's own runs of the
// same binary, such as the routing module's api commands, are not it.
func Find(proc string) (Xray, error) {
	dirs, err := os.ReadDir(proc)
	if err != nil {
		return Xray{}, err
	}
	self := strconv.Itoa(os.Getpid())
	var pids []string
	for _, d := range dirs {
		if _, err := strconv.Atoi(d.Name()); err != nil {
			continue
		}
		// 3x-ui names its binary xray-<os>-<arch>.
		if exe, err := os.Readlink(filepath.Join(proc, d.Name(), "exe")); err == nil && strings.HasPrefix(filepath.Base(exe), "xray-linux-") &&
			parent(filepath.Join(proc, d.Name())) != self {
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

// parent is the process ID of a process's parent, empty when unreadable. The
// command name in stat may hold spaces and parentheses, so fields count from
// its closing one.
func parent(dir string) string {
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
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

// PID identifies the process; a restart starts another.
func (x Xray) PID() string { return filepath.Base(x.dir) }

// Binary is the program the process runs, for running the same Xray again.
func (x Xray) Binary() (string, error) {
	return os.Readlink(filepath.Join(x.dir, "exe"))
}

// AssetEnv is where the process finds its geoip and geosite files, as an
// environment entry for another run of the same binary; nil means the
// binary's own directory, Xray's default.
func (x Xray) AssetEnv() ([]string, error) {
	key, value, found, err := x.flag("xray.location.asset", "XRAY_LOCATION_ASSET")
	if err != nil || !found {
		return nil, err
	}
	return []string{key + "=" + value}, nil
}

// Spliced says whether Xray hands the downlink of a Vision connection to the
// kernel, as the environment it started with decides. It then adds that
// connection's bytes to its user statistics only when the connection ends.
// Set to anything but auto or enable, even empty, it is off.
func (x Xray) Spliced() (bool, error) {
	_, value, found, err := x.flag("xray.buf.splice", "XRAY_BUF_SPLICE")
	if err != nil || !found {
		return true, err
	}
	return value == "auto" || value == "enable", nil
}

// flag reads one Xray environment flag, which Xray looks up by its own name,
// then by its environment name. Only these entries are read out of the
// environment, which may hold secrets.
func (x Xray) flag(keys ...string) (key, value string, found bool, err error) {
	environ, err := os.ReadFile(filepath.Join(x.dir, "environ"))
	if err != nil {
		return "", "", false, err
	}
	for _, key := range keys {
		for _, entry := range bytes.Split(environ, []byte{0}) {
			if v, ok := bytes.CutPrefix(entry, []byte(key+"=")); ok {
				return key, string(v), true, nil
			}
		}
	}
	return "", "", false, nil
}
