package accesslog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// running is what the configuration of the Xray that 3x-ui runs says: where
// it writes the access log, and the tags of the outbounds that drop whatever
// is sent to them.
type running struct {
	log        string
	blackholes map[string]bool
}

// locate finds the one Xray process 3x-ui runs and reads its configuration.
// A log that is off or masks addresses cannot say who a connection belongs
// to. 3x-ui restarts Xray to change outbounds, so the file names them as they
// run.
func locate(proc string) (running, error) {
	dirs, err := os.ReadDir(proc)
	if err != nil {
		return running{}, err
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
		return running{}, fmt.Errorf("found %d Xray processes started by 3x-ui", len(pids))
	}
	dir := filepath.Join(proc, pids[0])
	cwd, err := os.Readlink(filepath.Join(dir, "cwd"))
	if err != nil {
		return running{}, err
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return running{}, err
	}
	config := configArg(strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00"))
	if config == "" {
		return running{}, errors.New("the Xray command line names no configuration file")
	}
	raw, err := os.ReadFile(resolve(cwd, config))
	if err != nil {
		return running{}, err
	}
	var doc struct {
		Log struct {
			Access      string `json:"access"`
			MaskAddress string `json:"maskAddress"`
		} `json:"log"`
		Outbounds []struct {
			Tag      string `json:"tag"`
			Protocol string `json:"protocol"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return running{}, errors.New("the Xray configuration is not valid JSON")
	}
	if doc.Log.Access == "" || doc.Log.Access == "none" {
		return running{}, errors.New("the Xray access log is off")
	}
	if doc.Log.MaskAddress != "" {
		return running{}, errors.New("the Xray access log masks addresses")
	}
	found := running{log: resolve(cwd, doc.Log.Access), blackholes: map[string]bool{}}
	for _, o := range doc.Outbounds {
		if o.Protocol == "blackhole" && o.Tag != "" {
			found.blackholes[o.Tag] = true
		}
	}
	return found, nil
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

// resolve reads a path the way the process in dir would.
func resolve(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}
