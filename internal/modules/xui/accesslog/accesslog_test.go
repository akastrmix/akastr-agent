package accesslog

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAcceptedUserLines(t *testing.T) {
	cases := []struct {
		line, addr, email string
	}{
		{"2026/10/12 12:42:51.677607 from 203.0.113.7:33704 accepted tcp:example.com:443 [inbound-1 >> direct] email: ak-a1", "203.0.113.7:33704", "ak-a1"},
		{"2026/10/12 12:42:51.677607 from tcp:203.0.113.7:4000 accepted udp:8.8.8.8:53 [inbound-1 -> direct] email: ak-b2", "203.0.113.7:4000", "ak-b2"},
		{"2026/10/12 12:42:51.677607 from [2001:db8::7]:5000 accepted tcp:example.com:443 email: old-user", "[2001:db8::7]:5000", "old-user"},
		{"2026/10/12 12:42:51.677607 from [::ffff:203.0.113.7]:6000 accepted tcp:example.com:443 email: ak-c3", "203.0.113.7:6000", "ak-c3"},
	}
	for _, c := range cases {
		addr, email, ok := parse([]byte(c.line))
		if !ok || addr != netip.MustParseAddrPort(c.addr) || email != c.email {
			t.Fatalf("parse(%q) = %v %q %v", c.line, addr, email, ok)
		}
	}
	for _, line := range []string{
		"2026/10/12 12:42:51.677607 from 203.0.113.7:33704 rejected  proxy/vless/encoding: invalid request user id",
		"2026/10/12 12:42:51.677607 from 127.0.0.1:40000 accepted tcp:127.0.0.1:62789 [api -> api]",
		"2026/10/12 12:42:51.677607 from 203.0.113.7:0 accepted tcp:example.com:443 email: ak-a1",
		"from 203.0.113.7:1 accepted tcp:example.com:443 email: ",
		"half a line",
	} {
		if _, _, ok := parse([]byte(line)); ok {
			t.Fatalf("parse accepted %q", line)
		}
	}
}

func lineFor(addr, email string) string {
	return "2026/10/12 12:00:00.000000 from " + addr + " accepted tcp:example.com:443 [in >> out] email: " + email + "\n"
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// The follower must keep up with Xray appending and 3x-ui truncating in place,
// including a truncation that grows back past the old offset before a poll,
// and with the file being replaced.
func TestFollowerSurvivesTruncationAndReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	appendTo(t, path, lineFor("203.0.113.1:1", "ak-a"))
	f := &follower{path: path}
	defer f.close()
	var got []string
	poll := func() []string {
		got = got[:0]
		if err := f.poll(func(b []byte) {
			_, email, _ := parse(b)
			got = append(got, email)
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if e := poll(); strings.Join(e, ",") != "ak-a" {
		t.Fatalf("first read %v", e)
	}
	// Half a line waits for its end.
	full := lineFor("203.0.113.2:2", "ak-b")
	appendTo(t, path, full[:20])
	if e := poll(); len(e) != 0 {
		t.Fatalf("partial line emitted %v", e)
	}
	appendTo(t, path, full[20:])
	if e := poll(); strings.Join(e, ",") != "ak-b" {
		t.Fatalf("completed line %v", e)
	}
	// Truncated to empty, then a shorter text.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, lineFor("203.0.113.3:3", "ak-c"))
	if e := poll(); strings.Join(e, ",") != "ak-c" {
		t.Fatalf("after truncation %v", e)
	}
	// Truncated and grown past the old offset before the next poll.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, lineFor("203.0.113.4:4", "ak-d")+lineFor("203.0.113.5:5", "ak-e")+lineFor("203.0.113.6:6", "ak-f"))
	if e := poll(); strings.Join(e, ",") != "ak-d,ak-e,ak-f" {
		t.Fatalf("after truncation that grew back %v", e)
	}
	// Replaced by a new file.
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, lineFor("203.0.113.7:7", "ak-g"))
	if e := poll(); strings.Join(e, ",") != "ak-g" {
		t.Fatalf("after replacement %v", e)
	}
}

// fakeXray lays out what locate reads under /proc for 3x-ui's Xray.
func fakeXray(t *testing.T, logSection string) (proc, dir string) {
	t.Helper()
	root := t.TempDir()
	proc, dir = filepath.Join(root, "proc"), filepath.Join(root, "x-ui")
	pid := filepath.Join(proc, "4242")
	for _, d := range []string{pid, filepath.Join(dir, "bin"), filepath.Join(proc, "net"), filepath.Join(proc, "self")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink(filepath.Join(dir, "bin", "xray-linux-amd64"), filepath.Join(pid, "exe")))
	must(os.Symlink(dir, filepath.Join(pid, "cwd")))
	must(os.WriteFile(filepath.Join(pid, "cmdline"), []byte("bin/xray-linux-amd64\x00-c\x00bin/config.json\x00"), 0o600))
	must(os.WriteFile(filepath.Join(dir, "bin", "config.json"), []byte(`{"log":`+logSection+
		`,"inbounds":[{"port":62789},{"port":443},{"port":"8443"},{"port":"1000-2000"}]}`), 0o600))
	header := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	must(os.WriteFile(filepath.Join(proc, "net", "tcp"), []byte(header), 0o600))
	must(os.WriteFile(filepath.Join(proc, "net", "tcp6"), []byte(header), 0o600))
	return proc, dir
}

func TestLocateFollowsTheRunningConfiguration(t *testing.T) {
	proc, dir := fakeXray(t, `{"access":"./access.log","maskAddress":""}`)
	if path, ports, err := locate(proc); err != nil || path != filepath.Join(dir, "access.log") ||
		len(ports) != 3 || !ports[443] || !ports[8443] || !ports[62789] {
		t.Fatalf("locate = %q, %v, %v", path, ports, err)
	}
	for _, section := range []string{`{"access":"none"}`, `{"access":""}`, `{"access":"./access.log","maskAddress":"quarter"}`} {
		proc, _ := fakeXray(t, section)
		if _, _, err := locate(proc); err == nil {
			t.Fatalf("locate accepted %s", section)
		}
	}
	// A second Xray from 3x-ui is ambiguous.
	other := filepath.Join(proc, "4343")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/local/x-ui/bin/xray-linux-amd64", filepath.Join(other, "exe")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := locate(proc); err == nil {
		t.Fatal("locate chose between two Xray processes")
	}
}

func TestProcAddressesAndPruning(t *testing.T) {
	for field, want := range map[string]string{
		"0201A8C0:D431":                         "192.168.1.2:54321",
		"0000000000000000FFFF00000201A8C0:D431": "192.168.1.2:54321",
		"B80D0120000000000000000001000000:01BB": "[2001:db8::1]:443",
	} {
		if got, ok := procAddr(field); !ok || got != netip.MustParseAddrPort(want) {
			t.Fatalf("procAddr(%s) = %v %v", field, got, ok)
		}
	}
	l := newLog("")
	now := time.Now()
	quietLive, quietGone, recent := netip.MustParseAddrPort("192.168.1.2:54321"), netip.MustParseAddrPort("192.168.1.3:1"), netip.MustParseAddrPort("192.168.1.4:1")
	l.add([]line{{quietLive, "ak-a"}, {quietGone, "ak-a"}}, now.Add(-3*time.Hour))
	l.add([]line{{recent, "ak-b"}}, now)
	l.prune(now, map[netip.AddrPort]bool{quietLive: true})
	got := l.Connections(map[string]bool{"ak-a": true, "ak-b": true})
	if len(got["ak-a"]) != 1 || got["ak-a"][0] != quietLive || len(got["ak-b"]) != 1 {
		t.Fatalf("after prune %v", got)
	}
	// A reused address belongs to its newest user.
	l.add([]line{{recent, "ak-a"}}, now)
	if got := l.Connections(map[string]bool{"ak-b": true}); len(got) != 0 {
		t.Fatalf("reused address kept its old user %v", got)
	}
}

// Connections from the live log and, when still open, from the copy 3x-ui
// moved aside before the process started are known; the live log wins.
func TestRunMapsConnectionsFromTheLog(t *testing.T) {
	proc, dir := fakeXray(t, `{"access":"./access.log","maskAddress":""}`)
	appendTo(t, filepath.Join(dir, "access.log"), lineFor("203.0.113.9:9", "ak-z")+lineFor("192.168.1.2:54321", "ak-z"))
	appendTo(t, filepath.Join(dir, "copy.log"), lineFor("192.168.1.3:1", "ak-old")+lineFor("192.168.1.4:1", "ak-gone")+lineFor("192.168.1.2:54321", "ak-old"))
	appendTo(t, filepath.Join(proc, "net", "tcp"), "   0: 0100007F:1F90 0201A8C0:D431 01 00000000:00000000 00:00000000 00000000     0        0 1 1\n"+
		"   1: 0100007F:1F90 0301A8C0:0001 01 00000000:00000000 00:00000000 00000000     0        0 2 1\n")
	l := newLog(proc)
	l.copy = filepath.Join(dir, "copy.log")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- l.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got := l.Connections(map[string]bool{"ak-z": true, "ak-old": true, "ak-gone": true})
		if len(got["ak-z"]) == 2 && l.Err() == nil {
			if len(got["ak-old"]) != 1 || got["ak-old"][0] != netip.MustParseAddrPort("192.168.1.3:1") || len(got["ak-gone"]) != 0 {
				t.Fatalf("seeded connections %v", got)
			}
			// The usage meter asks per connection: on an inbound port, from whom.
			if email, ok := l.Owner(netip.MustParseAddrPort("203.0.113.9:9")); !ok || email != "ak-z" {
				t.Fatalf("owner %q %v", email, ok)
			}
			if !l.Inbound(netip.MustParseAddrPort("198.51.100.1:443")) || l.Inbound(netip.MustParseAddrPort("198.51.100.1:22")) {
				t.Fatal("inbound ports not taken from the running configuration")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("connection not mapped; err %v", l.Err())
}
