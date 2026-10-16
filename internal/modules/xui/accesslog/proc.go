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

// locate finds the one Xray process 3x-ui runs and the access log its
// configuration writes. A log that is off or masks addresses cannot say who a
// connection belongs to.
func locate(proc string) (string, error) {
	dirs, err := os.ReadDir(proc)
	if err != nil {
		return "", err
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
		return "", fmt.Errorf("found %d Xray processes started by 3x-ui", len(pids))
	}
	dir := filepath.Join(proc, pids[0])
	cwd, err := os.Readlink(filepath.Join(dir, "cwd"))
	if err != nil {
		return "", err
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return "", err
	}
	config := configArg(strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00"))
	if config == "" {
		return "", errors.New("the Xray command line names no configuration file")
	}
	raw, err := os.ReadFile(resolve(cwd, config))
	if err != nil {
		return "", err
	}
	var doc struct {
		Log struct {
			Access      string `json:"access"`
			MaskAddress string `json:"maskAddress"`
		} `json:"log"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", errors.New("the Xray configuration is not valid JSON")
	}
	if doc.Log.Access == "" || doc.Log.Access == "none" {
		return "", errors.New("the Xray access log is off")
	}
	if doc.Log.MaskAddress != "" {
		return "", errors.New("the Xray access log masks addresses")
	}
	return resolve(cwd, doc.Log.Access), nil
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
