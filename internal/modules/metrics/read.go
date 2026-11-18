package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/akastrmix/akastr-agent/internal/hostroute"
)

const (
	maxInterfaces = 16
	detectVirt    = "/usr/bin/systemd-detect-virt"
)

// sample is the data of metrics.sample.
type sample struct {
	BootID        string      `json:"boot_id"`
	UptimeSeconds int64       `json:"uptime_seconds"`
	CPU           *float64    `json:"cpu"`
	Load          [3]float64  `json:"load"`
	Memory        usage       `json:"memory"`
	Swap          usage       `json:"swap"`
	Disk          usage       `json:"disk"`
	Connections   connections `json:"connections"`
	Processes     int         `json:"processes"`
	Interfaces    []iface     `json:"interfaces"`
	System        system      `json:"system"`
}

type usage struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

type connections struct {
	TCP int64 `json:"tcp"`
	UDP int64 `json:"udp"`
}

// iface holds an interface's counters since boot.
type iface struct {
	Name    string `json:"name"`
	RXBytes uint64 `json:"rx_bytes"`
	TXBytes uint64 `json:"tx_bytes"`
}

type system struct {
	OS             *string `json:"os"`
	Kernel         string  `json:"kernel"`
	Arch           string  `json:"arch"`
	CPUModel       *string `json:"cpu_model"`
	CPUCores       int     `json:"cpu_cores"`
	Virtualization *string `json:"virtualization"`
}

// host reads the machine. Tests point the paths at a fake /proc and /sys.
type host struct {
	proc, sys, osRelease string
	filesystem           func(path string) (filesystem, error)
	machine              func() (string, error)
	virtualization       func(context.Context) *string
}

// filesystem is what statfs says about the filesystem at a mount point, and
// the device the mount point is on.
type filesystem struct {
	device, blocks, free, fragment uint64
}

func newHost() host {
	return host{
		proc: "/proc", sys: "/sys", osRelease: "/etc/os-release",
		filesystem: statFilesystem, machine: uname, virtualization: detectVirtualization,
	}
}

// cpuTimes are the jiffies /proc/stat counts for all CPUs since boot.
type cpuTimes struct{ idle, total uint64 }

func (h host) cpuTimes() (cpuTimes, error) {
	data, err := os.ReadFile(filepath.Join(h.proc, "stat"))
	if err != nil {
		return cpuTimes{}, err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(line)
	if len(fields) < 9 || fields[0] != "cpu" {
		return cpuTimes{}, errors.New("cpu line malformed")
	}
	// user nice system idle iowait irq softirq steal; guest time is already
	// part of user and nice.
	var times cpuTimes
	for i := 1; i <= 8; i++ {
		value, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return cpuTimes{}, fmt.Errorf("cpu line malformed: %w", err)
		}
		times.total += value
		if i == 4 || i == 5 {
			times.idle += value
		}
	}
	return times, nil
}

// busy is the percentage of CPU time not idle between two readings, to one
// decimal, or nil when no time passed. iowait may run backwards on some
// kernels, so the result is clamped instead of trusted.
func busy(previous, current cpuTimes) *float64 {
	total := float64(current.total) - float64(previous.total)
	if total <= 0 {
		return nil
	}
	idle := float64(current.idle) - float64(previous.idle)
	percent := math.Round((1-idle/total)*1000) / 10
	percent = math.Min(100, math.Max(0, percent))
	return &percent
}

func (h host) memory() (memory, swap usage, err error) {
	data, err := os.ReadFile(filepath.Join(h.proc, "meminfo"))
	if err != nil {
		return usage{}, usage{}, err
	}
	values := map[string]uint64{}
	for line := range strings.Lines(string(data)) {
		key, rest, ok := strings.Cut(line, ":")
		fields := strings.Fields(rest)
		if !ok || len(fields) == 0 {
			continue
		}
		if value, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
			values[key] = value * 1024
		}
	}
	for _, key := range []string{"MemTotal", "MemAvailable", "SwapTotal", "SwapFree"} {
		if _, ok := values[key]; !ok {
			return usage{}, usage{}, fmt.Errorf("meminfo has no %s", key)
		}
	}
	memory = usage{Total: values["MemTotal"], Used: values["MemTotal"] - min(values["MemAvailable"], values["MemTotal"])}
	swap = usage{Total: values["SwapTotal"], Used: values["SwapTotal"] - min(values["SwapFree"], values["SwapTotal"])}
	return memory, swap, nil
}

// disk adds up the filesystems on block devices, once per device: a sandbox
// or a bind mount shows the same filesystem at several mount points. Only
// reading the mount table can fail it.
func (h host) disk() (usage, error) {
	data, err := os.ReadFile(filepath.Join(h.proc, "self", "mounts"))
	if err != nil {
		return usage{}, err
	}
	var total usage
	seen := map[uint64]bool{}
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "/dev/") || strings.HasPrefix(fields[0], "/dev/loop") {
			continue
		}
		fs, err := h.filesystem(unescapeMount(fields[1]))
		if err != nil {
			// The sandbox hides mounts under /home, /root and /run/user;
			// one that cannot be reached is left out, not the whole sample.
			continue
		}
		if seen[fs.device] {
			continue
		}
		seen[fs.device] = true
		total.Total += fs.blocks * fs.fragment
		total.Used += (fs.blocks - min(fs.free, fs.blocks)) * fs.fragment
	}
	return total, nil
}

// unescapeMount undoes the octal escapes of /proc/self/mounts (\040 for a
// space and so on).
func unescapeMount(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] == '\\' && i+3 < len(path) {
			if value, err := strconv.ParseUint(path[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(value))
				i += 3
				continue
			}
		}
		b.WriteByte(path[i])
	}
	return b.String()
}

func statFilesystem(path string) (filesystem, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return filesystem{}, fmt.Errorf("stat %s: %w", path, err)
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return filesystem{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	fragment := uint64(fs.Frsize)
	if fragment == 0 {
		fragment = uint64(fs.Bsize)
	}
	return filesystem{device: st.Dev, blocks: fs.Blocks, free: fs.Bfree, fragment: fragment}, nil
}

// connections counts the sockets in use, without TIME-WAIT.
func (h host) connections() (connections, error) {
	data, err := os.ReadFile(filepath.Join(h.proc, "net", "sockstat"))
	if err != nil {
		return connections{}, err
	}
	var c connections
	if c.TCP, err = inUse(data, "TCP:"); err != nil {
		return connections{}, err
	}
	if c.UDP, err = inUse(data, "UDP:"); err != nil {
		return connections{}, err
	}
	data, err = os.ReadFile(filepath.Join(h.proc, "net", "sockstat6"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil // IPv6 is off
	}
	if err != nil {
		return connections{}, err
	}
	tcp6, err := inUse(data, "TCP6:")
	if err != nil {
		return connections{}, err
	}
	udp6, err := inUse(data, "UDP6:")
	if err != nil {
		return connections{}, err
	}
	c.TCP, c.UDP = c.TCP+tcp6, c.UDP+udp6
	return c, nil
}

// inUse reads "<label> inuse N ..." from a sockstat file.
func inUse(data []byte, label string) (int64, error) {
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == label && fields[1] == "inuse" {
			return strconv.ParseInt(fields[2], 10, 64)
		}
	}
	return 0, fmt.Errorf("sockstat has no %s inuse", label)
}

func (h host) processes() (int, error) {
	entries, err := os.ReadDir(h.proc)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if _, err := strconv.ParseUint(entry.Name(), 10, 64); err == nil {
			count++
		}
	}
	return count, nil
}

// interfaces are the network interfaces backed by a device: virtual ones
// (docker, veth, WireGuard, tun, bridges, bonds) have none, and counting them
// too would count their traffic twice. A container VPS has no device at all;
// there the interfaces of the default routes are counted instead.
func (h host) interfaces() ([]iface, error) {
	entries, err := os.ReadDir(filepath.Join(h.sys, "class", "net"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if _, err := os.Stat(filepath.Join(h.sys, "class", "net", entry.Name(), "device")); err == nil {
			names = append(names, entry.Name())
		}
	}
	if len(names) == 0 {
		v4, v6, err := hostroute.Defaults(h.proc)
		if err != nil {
			return nil, err
		}
		names = append(v4, v6...)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) > maxInterfaces {
		names = names[:maxInterfaces]
	}
	result := make([]iface, 0, len(names))
	for _, name := range names {
		statistics := filepath.Join(h.sys, "class", "net", name, "statistics")
		rx, err := readCounter(filepath.Join(statistics, "rx_bytes"))
		if err != nil {
			return nil, err
		}
		tx, err := readCounter(filepath.Join(statistics, "tx_bytes"))
		if err != nil {
			return nil, err
		}
		result = append(result, iface{Name: name, RXBytes: rx, TXBytes: tx})
	}
	return result, nil
}

func readCounter(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

func (h host) uptime() (int64, error) {
	data, err := os.ReadFile(filepath.Join(h.proc, "uptime"))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, errors.New("uptime empty")
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return int64(seconds), nil
}

func (h host) load() ([3]float64, error) {
	var load [3]float64
	data, err := os.ReadFile(filepath.Join(h.proc, "loadavg"))
	if err != nil {
		return load, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return load, errors.New("loadavg malformed")
	}
	for i := range load {
		if load[i], err = strconv.ParseFloat(fields[i], 64); err != nil {
			return load, err
		}
	}
	return load, nil
}

func readLine(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return line, nil
}

// system reads what describes the machine. virtualization is passed in: it
// is detected once per process.
func (h host) system(virtualization *string) (system, error) {
	s := system{OS: h.osName(), CPUModel: h.cpuModel(), Virtualization: virtualization}
	var err error
	if s.Kernel, err = readLine(filepath.Join(h.proc, "sys", "kernel", "osrelease")); err != nil {
		return system{}, err
	}
	if s.Arch, err = h.machine(); err != nil {
		return system{}, err
	}
	online, err := readLine(filepath.Join(h.sys, "devices", "system", "cpu", "online"))
	if err != nil {
		return system{}, err
	}
	if s.CPUCores, err = countCPUs(online); err != nil {
		return system{}, err
	}
	return s, nil
}

func (h host) osName() *string {
	data, err := os.ReadFile(h.osRelease)
	if err != nil {
		return nil
	}
	for line := range strings.Lines(string(data)) {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "PRETTY_NAME=")
		if !ok {
			continue
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		} else {
			value = strings.Trim(value, `'"`)
		}
		if value == "" {
			return nil
		}
		return &value
	}
	return nil
}

func (h host) cpuModel() *string {
	data, err := os.ReadFile(filepath.Join(h.proc, "cpuinfo"))
	if err != nil {
		return nil
	}
	for line := range strings.Lines(string(data)) {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(key) == "model name" {
			if value = strings.TrimSpace(value); value != "" {
				return &value
			}
			return nil
		}
	}
	return nil
}

// countCPUs counts a CPU list such as "0-3,8".
func countCPUs(list string) (int, error) {
	count := 0
	for part := range strings.SplitSeq(list, ",") {
		first, last, isRange := strings.Cut(part, "-")
		start, err := strconv.Atoi(first)
		if err != nil {
			return 0, fmt.Errorf("cpu list %q malformed", list)
		}
		end := start
		if isRange {
			if end, err = strconv.Atoi(last); err != nil || end < start {
				return 0, fmt.Errorf("cpu list %q malformed", list)
			}
		}
		count += end - start + 1
	}
	return count, nil
}

func uname() (string, error) {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return "", err
	}
	var machine []byte
	for _, c := range u.Machine {
		if c == 0 {
			break
		}
		machine = append(machine, byte(c))
	}
	if len(machine) == 0 {
		return "", errors.New("uname has no machine")
	}
	return string(machine), nil
}

var virtualizationName = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// detectVirtualization asks systemd, which prints "none" and exits 1 on a
// machine that is not virtualized.
func detectVirtualization(ctx context.Context) *string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, detectVirt).Output()
	name := strings.TrimSpace(string(output))
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1 && name == "none") {
		return nil
	}
	if !virtualizationName.MatchString(name) {
		return nil
	}
	return &name
}
