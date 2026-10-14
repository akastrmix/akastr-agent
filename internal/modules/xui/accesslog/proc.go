package accesslog

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// locate finds the one Xray process 3x-ui runs and the access log its
// configuration writes. A log that is off or masks addresses cannot say who a
// connection belongs to.
func locate(proc string) (string, map[uint16]bool, error) {
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
			Port json.RawMessage `json:"port"`
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
	// 3x-ui writes each port as a number. Ranges and environment references
	// are skipped: connections on them stay unknown.
	ports := map[uint16]bool{}
	for _, ib := range doc.Inbounds {
		if port, err := strconv.ParseUint(strings.Trim(string(ib.Port), `"`), 10, 16); err == nil && port > 0 {
			ports[uint16(port)] = true
		}
	}
	return resolve(cwd, doc.Log.Access), ports, nil
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

// liveTCP returns the remote address of every TCP connection the kernel holds.
func liveTCP(proc string) (map[netip.AddrPort]bool, error) {
	live := map[netip.AddrPort]bool{}
	for _, name := range []string{"tcp", "tcp6"} {
		file, err := os.Open(filepath.Join(proc, "net", name))
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Scan() // header
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			// The state LISTEN (0A) has no remote end.
			if len(fields) < 4 || fields[3] == "0A" {
				continue
			}
			if addr, ok := procAddr(fields[2]); ok {
				live[addr] = true
			}
		}
		err = scanner.Err()
		file.Close()
		if err != nil {
			return nil, err
		}
	}
	return live, nil
}

// procAddr decodes an address of /proc/net/tcp{,6}: the address as 32-bit
// words in host (little-endian) order, a colon and the port in hex.
func procAddr(field string) (netip.AddrPort, bool) {
	host, portText, found := strings.Cut(field, ":")
	raw, err := hex.DecodeString(host)
	port, perr := strconv.ParseUint(portText, 16, 16)
	if !found || err != nil || perr != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, false
	}
	for i := 0; i < len(raw); i += 4 {
		binary.BigEndian.PutUint32(raw[i:], binary.LittleEndian.Uint32(raw[i:]))
	}
	addr, _ := netip.AddrFromSlice(raw)
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)), true
}
