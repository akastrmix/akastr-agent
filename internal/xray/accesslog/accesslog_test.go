package accesslog

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseAcceptedUserLines(t *testing.T) {
	cases := []struct {
		line, tag, addr, email string
		dest                   Dest
		outbound               string
	}{
		{"2026/10/12 12:42:51.677607 from 203.0.113.7:33704 accepted tcp:example.com:443 [inbound-1 >> direct] email: ak-a1", "inbound-1", "203.0.113.7:33704", "ak-a1", Dest{"tcp", "example.com", 443}, "direct"},
		{"2026/10/12 12:42:51.677607 from tcp:203.0.113.7:4000 accepted udp:8.8.8.8:53 [inbound-2 -> direct] email: ak-b2", "inbound-2", "203.0.113.7:4000", "ak-b2", Dest{"udp", "8.8.8.8", 53}, "direct"},
		{"2026/10/12 12:42:51.677607 from [::ffff:203.0.113.7]:6000 accepted tcp:[2001:db8::9]:80 [inbound-3 ==> blocked] email: ak-c3", "inbound-3", "203.0.113.7:6000", "ak-c3", Dest{"tcp", "2001:db8::9", 80}, "blocked"},
		// Shadowsocks 2022 names no network.
		{"2026/10/12 12:42:51.677607 from 203.0.113.7:8000 accepted api.example:443 [inbound-5 >> direct] email: ak-e5", "inbound-5", "203.0.113.7:8000", "ak-e5", Dest{"", "api.example", 443}, "direct"},
		{"2026/10/12 12:42:51.677607 from 203.0.113.7:8001 accepted [2001:db8::9]:53 [inbound-5 >> direct] email: ak-e5", "inbound-5", "203.0.113.7:8001", "ak-e5", Dest{"", "2001:db8::9", 53}, "direct"},
		// A destination it cannot read still says whose the connection is.
		{"2026/10/12 12:42:51.677607 from 203.0.113.7:7000 accepted unknown [inbound-4 >> direct] email: ak-d4", "inbound-4", "203.0.113.7:7000", "ak-d4", Dest{}, "direct"},
		{"2026/10/12 12:42:51.677607 from 203.0.113.7:7001 accepted tcp:ex\"<a>mple.com:443 [inbound-4 >> direct] email: ak-d4", "inbound-4", "203.0.113.7:7001", "ak-d4", Dest{}, "direct"},
		{"2026/10/12 12:42:51.677607 from 203.0.113.7:7002 accepted tcp:.:443 [inbound-4 >> direct] email: ak-d4", "inbound-4", "203.0.113.7:7002", "ak-d4", Dest{}, "direct"},
	}
	stamp := time.Date(2026, 10, 12, 12, 42, 51, 677607000, time.Local)
	for _, c := range cases {
		e, ok := parse([]byte(c.line))
		if !ok || e.tag != c.tag || e.addr != netip.MustParseAddrPort(c.addr) || e.email != c.email || !e.at.Equal(stamp) ||
			e.dest != c.dest || e.outbound != c.outbound {
			t.Fatalf("parse(%q) = %+v %v", c.line, e, ok)
		}
	}
	if e, ok := parse([]byte("garbled from 203.0.113.7:1 accepted tcp:example.com:443 [inbound-1 >> direct] email: ak-a1")); !ok || !e.at.IsZero() {
		t.Fatalf("a line without a time = %+v %v", e, ok)
	}
	for _, line := range []string{
		"2026/10/12 12:42:51.677607 from 203.0.113.7:33704 rejected  proxy/vless/encoding: invalid request user id",
		"2026/10/12 12:42:51.677607 from 127.0.0.1:40000 accepted tcp:127.0.0.1:62789 [api -> api]",
		"2026/10/12 12:42:51.677607 from 203.0.113.7:0 accepted tcp:example.com:443 [inbound-1 >> direct] email: ak-a1",
		// Without the route the inbound, and so the connection, is unknown.
		"2026/10/12 12:42:51.677607 from [2001:db8::7]:5000 accepted tcp:example.com:443 email: old-user",
		"from 203.0.113.7:1 accepted tcp:example.com:443 [inbound-1 >> direct] email: ",
		"half a line",
	} {
		if _, ok := parse([]byte(line)); ok {
			t.Fatalf("parse accepted %q", line)
		}
	}
}

func lineFor(addr, email string) string {
	return lineAt("2026/10/12 12:00:00.000000", "inbound-443", addr, email)
}

func lineAt(stamp, tag, addr, email string) string {
	return stamp + " from " + addr + " accepted tcp:example.com:443 [" + tag + " >> direct] email: " + email + "\n"
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

// clearLog does what 3x-ui does to the log: append it to the copy, then
// truncate it in place.
func clearLog(t *testing.T, path, copy string) {
	t.Helper()
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	appendTo(t, copy, string(text))
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
}

// pollEmails polls f and returns the users of the lines, an old one marked
// with a star.
func pollEmails(t *testing.T, f *follower) string {
	t.Helper()
	var got []string
	if err := f.poll(func(b []byte, backlog bool) {
		e, _ := parse(b)
		if backlog {
			e.email += "*"
		}
		got = append(got, e.email)
	}); err != nil {
		t.Fatal(err)
	}
	return strings.Join(got, ",")
}

// The follower must keep up with Xray appending and 3x-ui truncating in place,
// including a truncation that grows back past the old offset before a poll,
// and with the file being replaced.
func TestFollowerSurvivesTruncationAndReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	appendTo(t, path, lineFor("203.0.113.1:1", "ak-a"))
	f := &follower{path: path}
	defer f.close()
	if e := pollEmails(t, f); e != "ak-a*" {
		t.Fatalf("first read %v", e)
	}
	// Half a line waits for its end.
	full := lineFor("203.0.113.2:2", "ak-b")
	appendTo(t, path, full[:20])
	if e := pollEmails(t, f); e != "" {
		t.Fatalf("partial line emitted %v", e)
	}
	appendTo(t, path, full[20:])
	if e := pollEmails(t, f); e != "ak-b" {
		t.Fatalf("completed line %v", e)
	}
	// Truncated to empty, then a shorter text.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, lineFor("203.0.113.3:3", "ak-c"))
	if e := pollEmails(t, f); e != "ak-c" {
		t.Fatalf("after truncation %v", e)
	}
	// Truncated and grown past the old offset before the next poll.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, lineFor("203.0.113.4:4", "ak-d")+lineFor("203.0.113.5:5", "ak-e")+lineFor("203.0.113.6:6", "ak-f"))
	if e := pollEmails(t, f); e != "ak-d,ak-e,ak-f" {
		t.Fatalf("after truncation that grew back %v", e)
	}
	// Replaced by a new file.
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, lineFor("203.0.113.7:7", "ak-g"))
	if e := pollEmails(t, f); e != "ak-g*" {
		t.Fatalf("after replacement %v", e)
	}
}

// Lines written after the last poll and before 3x-ui truncated the log are
// read back from its copy, once and in order. A copy that no longer matches
// what was read yields nothing rather than wrong lines.
func TestFollowerReadsBackWhatATruncationTook(t *testing.T) {
	dir := t.TempDir()
	path, copy := filepath.Join(dir, "access.log"), filepath.Join(dir, "3xipl-ap.log")
	appendTo(t, copy, lineFor("203.0.113.1:1", "ak-old"))
	appendTo(t, path, lineFor("203.0.113.2:2", "ak-a"))
	f := &follower{path: path, copy: copy}
	defer f.close()
	// Opened from its start, the log follows the end of the copy.
	if e := pollEmails(t, f); e != "ak-old*,ak-a*" {
		t.Fatalf("first read %v", e)
	}
	appendTo(t, path, lineFor("203.0.113.3:3", "ak-b"))
	clearLog(t, path, copy)
	appendTo(t, path, lineFor("203.0.113.4:4", "ak-c"))
	if e := pollEmails(t, f); e != "ak-b,ak-c" {
		t.Fatalf("after a truncation %v", e)
	}
	// Grown back past the old offset before the poll.
	appendTo(t, path, lineFor("203.0.113.5:5", "ak-d"))
	clearLog(t, path, copy)
	appendTo(t, path, lineFor("203.0.113.6:6", "ak-e")+lineFor("203.0.113.7:7", "ak-f")+lineFor("203.0.113.8:8", "ak-g"))
	if e := pollEmails(t, f); e != "ak-d,ak-e,ak-f,ak-g" {
		t.Fatalf("after a truncation that grew back %v", e)
	}
	// Two truncations between polls: both pieces come back.
	appendTo(t, path, lineFor("203.0.113.9:9", "ak-h"))
	clearLog(t, path, copy)
	appendTo(t, path, lineFor("203.0.113.10:10", "ak-i"))
	clearLog(t, path, copy)
	appendTo(t, path, lineFor("203.0.113.11:11", "ak-j"))
	if e := pollEmails(t, f); e != "ak-h,ak-i,ak-j" {
		t.Fatalf("after two truncations %v", e)
	}
	// A truncation while nothing was read since the last one.
	clearLog(t, path, copy)
	if e := pollEmails(t, f); e != "" {
		t.Fatalf("after an empty truncation %v", e)
	}
	appendTo(t, path, lineFor("203.0.113.16:16", "ak-n"))
	clearLog(t, path, copy)
	if e := pollEmails(t, f); e != "ak-n" {
		t.Fatalf("after a truncation from the start %v", e)
	}
	appendTo(t, path, lineFor("203.0.113.17:17", "ak-o"))
	clearLog(t, path, copy)
	appendTo(t, path, lineFor("203.0.113.18:18", "ak-p"))
	if e := pollEmails(t, f); e != "ak-o,ak-p" {
		t.Fatalf("after a truncation from the start that grew back %v", e)
	}
	// Copied but not yet truncated: the lines are read once, from the log.
	appendTo(t, path, lineFor("203.0.113.19:19", "ak-q"))
	text, _ := os.ReadFile(path)
	appendTo(t, copy, string(text))
	if e := pollEmails(t, f); e != "ak-q" {
		t.Fatalf("before the truncation %v", e)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if e := pollEmails(t, f); e != "" {
		t.Fatalf("lines read twice %v", e)
	}
	// The same with nothing read since the last truncation.
	appendTo(t, path, lineFor("203.0.113.20:20", "ak-r"))
	text, _ = os.ReadFile(path)
	appendTo(t, copy, string(text))
	if e := pollEmails(t, f); e != "ak-r" {
		t.Fatalf("before a truncation from the start %v", e)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if e := pollEmails(t, f); e != "" {
		t.Fatalf("lines read twice from the start %v", e)
	}
	// The copy is rotated before the truncation: nothing is read back.
	appendTo(t, path, lineFor("203.0.113.12:12", "ak-k"))
	if err := os.Rename(copy, copy+".prev"); err != nil {
		t.Fatal(err)
	}
	clearLog(t, path, copy)
	appendTo(t, path, lineFor("203.0.113.13:13", "ak-l"))
	if e := pollEmails(t, f); e != "ak-l" {
		t.Fatalf("after a rotated copy %v", e)
	}
	// The copy changed where the lines should be: nothing is read back.
	appendTo(t, path, lineFor("203.0.113.14:14", "ak-m"))
	appendTo(t, copy, lineFor("203.0.113.15:15", "ak-x"))
	clearLog(t, path, copy)
	if e := pollEmails(t, f); e != "" {
		t.Fatalf("after a changed copy %v", e)
	}
}

// fakeXray lays out what locate reads under /proc for 3x-ui's Xray, with
// 3x-ui's usual outbounds.
func fakeXray(t *testing.T, logSection string) (proc, dir string) {
	t.Helper()
	root := t.TempDir()
	proc, dir = filepath.Join(root, "proc"), filepath.Join(root, "x-ui")
	pid := filepath.Join(proc, "4242")
	for _, d := range []string{pid, filepath.Join(dir, "bin")} {
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
	must(os.WriteFile(filepath.Join(dir, "bin", "config.json"), []byte(`{"log":`+logSection+`,"inbounds":[],`+
		`"outbounds":[{"tag":"direct","protocol":"freedom"},{"tag":"blocked","protocol":"blackhole"},{"tag":"drop-ads","protocol":"blackhole"}]}`), 0o600))
	return proc, dir
}

func TestLocateFollowsTheRunningConfiguration(t *testing.T) {
	proc, dir := fakeXray(t, `{"access":"./access.log","maskAddress":""}`)
	if found, err := locate(proc); err != nil || found.log != filepath.Join(dir, "access.log") ||
		!reflect.DeepEqual(found.blackholes, map[string]bool{"blocked": true, "drop-ads": true}) {
		t.Fatalf("locate = %+v, %v", found, err)
	}
	for _, section := range []string{`{"access":"none"}`, `{"access":""}`, `{"access":"./access.log","maskAddress":"quarter"}`} {
		proc, _ := fakeXray(t, section)
		if _, err := locate(proc); err == nil {
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
	if _, err := locate(proc); err == nil {
		t.Fatal("locate chose between two Xray processes")
	}
}

// Run passes the lines already in the copy and the log, then new ones, on the
// inbound's port; a line of an inbound the panel added to the running Xray
// later is not lost. An old line keeps its time; a new line stamped on
// another clock counts as read now but keeps the time it carries. An old line
// of an inbound that takes TCP too is no UDP flow. A line sent to a blackhole
// outbound is blocked, whatever the outbound is called.
func TestRunPassesLines(t *testing.T) {
	proc, dir := fakeXray(t, `{"access":"./access.log","maskAddress":""}`)
	path, copy := filepath.Join(dir, "access.log"), filepath.Join(dir, "3xipl-ap.log")
	appendTo(t, copy, lineAt("2026/10/12 11:00:00.000000", "inbound-443", "203.0.113.1:1", "ak-a"))
	appendTo(t, path, lineAt("2026/10/12 11:30:00.000000", "inbound-8443", "203.0.113.2:2", "ak-b")+
		lineAt("2026/10/12 11:30:00.000000", "inbound-unknown", "203.0.113.3:3", "ak-c")+
		lineAt("2026/10/12 11:40:00.000000", "inbound-9443", "203.0.113.4:4", "ak-h"))
	var mu sync.Mutex
	inbounds := []Inbound{
		{Tag: "inbound-443", Protocol: "vless", Port: 443, Transport: "tcp"},
		{Tag: "inbound-8443", Protocol: "shadowsocks", Port: 8443, Network: "tcp,udp", Transport: "tcp"},
		{Tag: "inbound-9443", Protocol: "hysteria", Port: 9443, Transport: "hysteria"},
	}
	l := newLog(proc, copy, func(context.Context) ([]Inbound, error) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(inbounds), nil
	})
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.Local)
	l.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	batches := make(chan []Line, 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- l.Run(ctx, func(lines []Line) { batches <- lines }) }()
	defer func() { cancel(); <-done }()
	next := func() []Line {
		select {
		case lines := <-batches:
			return lines
		case <-time.After(3 * time.Second):
			t.Fatalf("no lines; err %v", l.Err())
			return nil
		}
	}
	web := Dest{"tcp", "example.com", 443}
	line := func(port uint16, addr, email string, at, written time.Time, backlog, udp bool) Line {
		return Line{Conn: Conn{port, netip.MustParseAddrPort(addr)}, Email: email, At: at, Written: written,
			Backlog: backlog, UDP: udp, Dest: web, Outbound: "direct"}
	}
	at := func(h, m, s, ms int) time.Time { return time.Date(2026, 10, 12, h, m, s, ms*1e6, time.Local) }
	want := []Line{
		line(443, "203.0.113.1:1", "ak-a", at(11, 0, 0, 0), at(11, 0, 0, 0), true, false),
		line(8443, "203.0.113.2:2", "ak-b", at(11, 30, 0, 0), at(11, 30, 0, 0), true, false),
		line(9443, "203.0.113.4:4", "ak-h", at(11, 40, 0, 0), at(11, 40, 0, 0), true, true),
	}
	if got := next(); !reflect.DeepEqual(got, want) || l.Err() != nil {
		t.Fatalf("first lines %+v, err %v", got, l.Err())
	}
	// An inbound added later is known from its first line on.
	mu.Lock()
	now = now.Add(30 * time.Second)
	inbounds = append(inbounds, Inbound{Tag: "inbound-7443", Protocol: "vless", Port: 7443, Transport: "tcp"})
	mu.Unlock()
	appendTo(t, path, lineAt("2026/10/12 07:59:59.500000", "inbound-7443", "203.0.113.8:8", "ak-new")+
		lineAt("2026/10/12 12:00:29.500000", "inbound-443", "203.0.113.9:9", "ak-d")+
		lineAt("2026/10/12 12:00:29.500000", "inbound-8443", "203.0.113.2:5", "ak-b")+
		"2026/10/12 12:00:29.600000 from 203.0.113.9:10 accepted tcp:10.0.0.1:80 [inbound-443 -> drop-ads] email: ak-d\n")
	blocked := line(443, "203.0.113.9:10", "ak-d", at(12, 0, 29, 600), at(12, 0, 29, 600), false, false)
	blocked.Dest, blocked.Outbound, blocked.Blocked = Dest{"tcp", "10.0.0.1", 80}, "drop-ads", true
	want = []Line{
		line(7443, "203.0.113.8:8", "ak-new", l.now(), at(7, 59, 59, 500), false, false),
		line(443, "203.0.113.9:9", "ak-d", at(12, 0, 29, 500), at(12, 0, 29, 500), false, false),
		line(8443, "203.0.113.2:5", "ak-b", at(12, 0, 29, 500), at(12, 0, 29, 500), false, true),
		blocked,
	}
	if got := next(); !reflect.DeepEqual(got, want) {
		t.Fatalf("new lines %+v", got)
	}
}
