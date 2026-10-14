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

// inbound is what the access log's tag of an inbound stands for.
type inbound struct {
	port uint16
	// tcp and udp are how clients can reach the inbound. The kernel's TCP
	// table does not show UDP.
	tcp, udp bool
}

// locate finds the one Xray process 3x-ui runs, the access log its
// configuration writes and its inbounds by tag. A log that is off or masks
// addresses cannot say who a connection belongs to.
func locate(proc string) (string, map[string]inbound, error) {
	dirs, err := os.ReadDir(proc)
	if err != nil {
		return "", nil, err
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
		return "", nil, fmt.Errorf("found %d Xray processes started by 3x-ui", len(pids))
	}
	dir := filepath.Join(proc, pids[0])
	cwd, err := os.Readlink(filepath.Join(dir, "cwd"))
	if err != nil {
		return "", nil, err
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return "", nil, err
	}
	config := configArg(strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00"))
	if config == "" {
		return "", nil, errors.New("the Xray command line names no configuration file")
	}
	raw, err := os.ReadFile(resolve(cwd, config))
	if err != nil {
		return "", nil, err
	}
	var doc struct {
		Log struct {
			Access      string `json:"access"`
			MaskAddress string `json:"maskAddress"`
		} `json:"log"`
		Inbounds []struct {
			Tag      string          `json:"tag"`
			Port     json.RawMessage `json:"port"`
			Protocol string          `json:"protocol"`
			Settings struct {
				Network string `json:"network"`
			} `json:"settings"`
			StreamSettings struct {
				Network string `json:"network"`
			} `json:"streamSettings"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", nil, errors.New("the Xray configuration is not valid JSON")
	}
	if doc.Log.Access == "" || doc.Log.Access == "none" {
		return "", nil, errors.New("the Xray access log is off")
	}
	if doc.Log.MaskAddress != "" {
		return "", nil, errors.New("the Xray access log masks addresses")
	}
	// 3x-ui tags each inbound and writes its port as a number. Ranges and
	// environment references are skipped: connections on them stay unknown.
	inbounds := map[string]inbound{}
	for _, ib := range doc.Inbounds {
		port, err := strconv.ParseUint(strings.Trim(string(ib.Port), `"`), 10, 16)
		if err != nil || port == 0 || ib.Tag == "" {
			continue
		}
		udpOnly := strings.HasPrefix(ib.Protocol, "hysteria") || ib.StreamSettings.Network == "kcp" || ib.StreamSettings.Network == "quic"
		tcp, udp := !udpOnly, udpOnly
		if network := ib.Settings.Network; network != "" && !udpOnly {
			tcp, udp = strings.Contains(network, "tcp"), strings.Contains(network, "udp")
		}
		inbounds[ib.Tag] = inbound{port: uint16(port), tcp: tcp, udp: udp}
	}
	return resolve(cwd, doc.Log.Access), inbounds, nil
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
