package metrics

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fakeHost builds a /proc and /sys tree under a temporary directory.
type fakeHost struct {
	t    *testing.T
	root string
	disk map[string]filesystem
}

func newFakeHost(t *testing.T) *fakeHost {
	f := &fakeHost{t: t, root: t.TempDir(), disk: map[string]filesystem{}}
	f.write("proc/stat", "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 100 0 100 700 100 0 0 0 0 0\n")
	f.write("proc/sys/kernel/random/boot_id", "52784529-8786-4cc3-8a3d-16085a0627bd\n")
	f.write("proc/uptime", "194245.67 190000.00\n")
	f.write("proc/loadavg", "0.08 0.02 0.01 1/120 4242\n")
	f.write("proc/meminfo", "MemTotal:        1498296 kB\nMemFree:          480228 kB\n"+
		"MemAvailable:    1044612 kB\nSwapCached:            0 kB\nSwapTotal:       1048572 kB\nSwapFree:         1048060 kB\n")
	f.write("proc/net/sockstat", "sockets: used 123\nTCP: inuse 8 orphan 0 tw 5 alloc 18 mem 2\n"+
		"UDP: inuse 3 mem 1\nUDPLITE: inuse 9\nRAW: inuse 0\n")
	f.write("proc/net/sockstat6", "TCP6: inuse 2\nUDP6: inuse 1\nUDPLITE6: inuse 7\nRAW6: inuse 0\n")
	f.write("proc/self/mounts", "/dev/vda1 / ext4 rw 0 0\n")
	f.write("proc/1/stat", "")
	f.write("proc/42/stat", "")
	f.write("proc/sys/kernel/osrelease", "6.1.0-42-amd64\n")
	f.write("proc/cpuinfo", "processor\t: 0\nmodel name\t: Intel(R) Xeon(R) CPU E5-2697 v2 @ 2.70GHz\n")
	f.write("sys/devices/system/cpu/online", "0\n")
	f.write("etc/os-release", "NAME=\"Debian GNU/Linux\"\nPRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n")
	f.write("proc/net/route", "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
		"eth0\t00000000\t0102A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n")
	f.nic("eth0", true, 8309313478, 7920090065)
	f.disk["/"] = filesystem{device: 65025, blocks: 8752364, free: 7679977, fragment: 4096}
	return f
}

func (f *fakeHost) write(name, content string) {
	f.t.Helper()
	path := filepath.Join(f.root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeHost) nic(name string, device bool, rx, tx uint64) {
	f.write("sys/class/net/"+name+"/statistics/rx_bytes", formatUint(rx))
	f.write("sys/class/net/"+name+"/statistics/tx_bytes", formatUint(tx))
	if device {
		f.write("sys/class/net/"+name+"/device/vendor", "0x1af4\n")
	}
}

func formatUint(v uint64) string {
	raw, _ := json.Marshal(v)
	return string(raw) + "\n"
}

func (f *fakeHost) host() host {
	return host{
		proc: filepath.Join(f.root, "proc"), sys: filepath.Join(f.root, "sys"),
		osRelease: filepath.Join(f.root, "etc/os-release"),
		filesystem: func(path string) (filesystem, error) {
			fs, ok := f.disk[path]
			if !ok {
				f.t.Fatalf("statfs of unexpected mount point %q", path)
			}
			return fs, nil
		},
		machine:        func() (string, error) { return "x86_64", nil },
		virtualization: func(context.Context) *string { kvm := "kvm"; return &kvm },
	}
}

func names(list []iface) []string {
	var result []string
	for _, i := range list {
		result = append(result, i.Name)
	}
	return result
}

// Counting a virtual interface on top of the real one would count its
// traffic twice and use up a traffic quota early.
func TestInterfacesAreTheOnesWithADevice(t *testing.T) {
	f := newFakeHost(t)
	f.nic("docker0", false, 5314384, 12115030)
	f.nic("veth04288c6", false, 6054662, 12119816)
	f.nic("wg0", false, 900, 900)
	f.nic("lo", false, 48689193, 48689193)
	f.nic("ens4", true, 10, 20)
	f.write("sys/class/net/bonding_masters", "bond0\n")
	got, err := f.host().interfaces()
	if err != nil {
		t.Fatal(err)
	}
	want := []iface{{Name: "ens4", RXBytes: 10, TXBytes: 20}, {Name: "eth0", RXBytes: 8309313478, TXBytes: 7920090065}}
	if !slices.Equal(got, want) {
		t.Fatalf("interfaces = %+v, want %+v", got, want)
	}
}

// A container VPS has no interface with a device: the default routes name
// the ones its traffic leaves through.
func TestInterfacesFallBackToTheDefaultRoutes(t *testing.T) {
	f := newFakeHost(t)
	if err := os.RemoveAll(filepath.Join(f.root, "sys/class/net/eth0/device")); err != nil {
		t.Fatal(err)
	}
	f.nic("venet0", false, 1, 2)
	f.nic("v6tun", false, 3, 4)
	f.nic("lo", false, 5, 5)
	f.write("proc/net/route", "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
		"venet0\t0000FEA9\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"+
		"venet0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n")
	zero := "00000000000000000000000000000000"
	f.write("proc/net/ipv6_route",
		zero+" 00 "+zero+" 00 "+zero+" 00000400 00000001 00000000 00000001   v6tun\n"+
			zero+" 00 "+zero+" 00 "+zero+" ffffffff 00000001 00000000 00200200       lo\n"+
			"fe800000000000000000000000000000 40 "+zero+" 00 "+zero+" 00000100 00000001 00000000 00000001   venet0\n")
	got, err := f.host().interfaces()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(got), []string{"v6tun", "venet0"}) {
		t.Fatalf("interfaces = %+v", got)
	}

	// No default route either: an empty list, not an error.
	f.write("proc/net/route", "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n")
	if err := os.Remove(filepath.Join(f.root, "proc/net/ipv6_route")); err != nil {
		t.Fatal(err)
	}
	got, err = f.host().interfaces()
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("interfaces = %+v, %v; want an empty list", got, err)
	}
}

// The Agent's sandbox mounts the root filesystem again at several places;
// each filesystem counts once, and loop devices (snaps, images) not at all.
func TestDiskCountsEachDeviceOnce(t *testing.T) {
	f := newFakeHost(t)
	f.write("proc/self/mounts", "/dev/vda1 / ext4 ro 0 0\n"+
		"udev /dev devtmpfs rw 0 0\n"+
		"/dev/vda1 /tmp ext4 rw 0 0\n"+
		"/dev/vda1 /var/lib/akastr-agent ext4 rw 0 0\n"+
		"/dev/vdb1 /srv/data\\040disk xfs rw 0 0\n"+
		"/dev/loop0 /snap/core/1 squashfs ro 0 0\n"+
		"tmpfs /run tmpfs rw 0 0\n"+
		"overlay /var/lib/docker/overlay2/x/merged overlay rw 0 0\n")
	root := f.disk["/"]
	f.disk["/tmp"] = root
	f.disk["/var/lib/akastr-agent"] = root
	f.disk["/srv/data disk"] = filesystem{device: 65041, blocks: 1000, free: 250, fragment: 4096}
	got, err := f.host().disk()
	if err != nil {
		t.Fatal(err)
	}
	want := usage{Total: (8752364 + 1000) * 4096, Used: (8752364 - 7679977 + 750) * 4096}
	if got != want {
		t.Fatalf("disk = %+v, want %+v", got, want)
	}
}

// A whole sample: the first has no CPU figure, the next one the busy share
// between the two; memory and sockets read as free and ss show them.
func TestSample(t *testing.T) {
	f := newFakeHost(t)
	detections := 0
	h := f.host()
	h.virtualization = func(context.Context) *string { detections++; return nil }
	s := &Sampler{host: h}
	first, err := s.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.CPU != nil {
		t.Fatalf("first cpu = %v, want null", *first.CPU)
	}
	// 1000 jiffies later, 250 of them idle or waiting for I/O.
	f.write("proc/stat", "cpu  500 100 200 850 200 50 50 50 30 0\n")
	second, err := s.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.CPU == nil || *second.CPU != 75 {
		t.Fatalf("cpu = %v, want 75", second.CPU)
	}
	if detections != 1 {
		t.Fatalf("systemd-detect-virt ran %d times, want once", detections)
	}
	raw, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"boot_id": "52784529-8786-4cc3-8a3d-16085a0627bd", "uptime_seconds": 194245.0, "cpu": 75.0,
		"load":   []any{0.08, 0.02, 0.01},
		"memory": map[string]any{"total": 1498296.0 * 1024, "used": (1498296.0 - 1044612) * 1024},
		"swap":   map[string]any{"total": 1048572.0 * 1024, "used": 512.0 * 1024},
		"disk":   map[string]any{"total": 8752364.0 * 4096, "used": (8752364.0 - 7679977) * 4096},
		// UDPLITE is not UDP.
		"connections": map[string]any{"tcp": 10.0, "udp": 4.0},
		"processes":   2.0,
		"interfaces":  []any{map[string]any{"name": "eth0", "rx_bytes": 8309313478.0, "tx_bytes": 7920090065.0}},
		"system": map[string]any{"os": "Debian GNU/Linux 12 (bookworm)", "kernel": "6.1.0-42-amd64", "arch": "x86_64",
			"cpu_model": "Intel(R) Xeon(R) CPU E5-2697 v2 @ 2.70GHz", "cpu_cores": 1.0, "virtualization": nil},
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("sample\n got %s\nwant %s", gotJSON, wantJSON)
	}

	// Without IPv6 there is no sockstat6; a missing os-release is null.
	for _, name := range []string{"proc/net/sockstat6", "etc/os-release"} {
		if err := os.Remove(filepath.Join(f.root, name)); err != nil {
			t.Fatal(err)
		}
	}
	third, err := s.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third.Connections != (connections{TCP: 8, UDP: 3}) || third.System.OS != nil {
		t.Fatalf("without sockstat6 and os-release: %+v, os %v", third.Connections, third.System.OS)
	}

	// Anything else unreadable skips the sample.
	if err := os.Remove(filepath.Join(f.root, "proc/meminfo")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.read(context.Background()); err == nil {
		t.Fatal("a sample without meminfo was taken")
	}
}

func TestBusyClampsCountersThatRunBackwards(t *testing.T) {
	if got := busy(cpuTimes{idle: 100, total: 1000}, cpuTimes{idle: 100, total: 1000}); got != nil {
		t.Fatalf("no time passed: %v, want null", *got)
	}
	// iowait went back more than the time that passed.
	if got := busy(cpuTimes{idle: 500, total: 1000}, cpuTimes{idle: 400, total: 1100}); got == nil || *got != 100 {
		t.Fatalf("busy = %v, want 100", got)
	}
	if got := busy(cpuTimes{idle: 0, total: 0}, cpuTimes{idle: 2, total: 3}); got == nil || *got != 33.3 {
		t.Fatalf("busy = %v, want 33.3", got)
	}
}

func TestCountCPUs(t *testing.T) {
	for list, want := range map[string]int{"0": 1, "0-3": 4, "0-3,8,10-11": 7} {
		if got, err := countCPUs(list); err != nil || got != want {
			t.Errorf("countCPUs(%q) = %d, %v; want %d", list, got, err, want)
		}
	}
	if _, err := countCPUs("3-1"); err == nil {
		t.Error("a backwards range was accepted")
	}
}
