package connectionrecords

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/xray/accesslog"
	"github.com/akastrmix/akastr-agent/internal/xray/ledger"
)

var written = time.Date(2026, 10, 26, 20, 15, 7, 123456789, time.FixedZone("HKT", 8*3600))

func line(email, source string) accesslog.Line {
	return accesslog.Line{Conn: accesslog.Conn{Port: 443, Remote: netip.MustParseAddrPort(source)}, Email: email,
		At: written, Written: written, Dest: accesslog.Dest{Network: "tcp", Host: "example.com", Port: 443}, Outbound: "direct"}
}

// lineAt is a line written seconds after written, read on opening the log
// when backlog is set.
func lineAt(seconds int, source string, backlog bool) accesslog.Line {
	l := line("ak-a", source)
	l.Written = written.Add(time.Duration(seconds) * time.Second)
	l.Backlog = backlog
	return l
}

// sent collects what the module publishes.
type sent []protocol.ReportBody

func (s *sent) publish(r protocol.ReportBody) error { *s = append(*s, r); return nil }

// decode reads a batch the way Cloud does: every field of a record present,
// the four of an end record all or none.
func decode(t *testing.T, r protocol.ReportBody) (records []map[string]any, gaps []gap) {
	t.Helper()
	if r.Kind != KindBatch {
		t.Fatalf("kind %q", r.Kind)
	}
	var b struct {
		Records []map[string]any `json:"records"`
		Gaps    []gap            `json:"gaps"`
	}
	raw := r.Data.(json.RawMessage)
	if err := json.Unmarshal(raw, &b); err != nil || b.Records == nil {
		t.Fatalf("batch %s: %v", raw, err)
	}
	for _, rec := range b.Records {
		if len(rec) != 8 && len(rec) != 12 {
			t.Fatalf("record fields %v", rec)
		}
	}
	return b.Records, b.Gaps
}

func sources(records []map[string]any) string {
	var out []string
	for _, rec := range records {
		out = append(out, rec["source"].(string))
	}
	return strings.Join(out, " ")
}

// A record is made of each new line of an owned client that says when and
// where; an end record carries the same line with what its connection did.
// The first start reads no old line again: there is no record of which were
// sent.
func TestRecordsOfLinesAndEndedConnections(t *testing.T) {
	r := New(t.TempDir())
	backlog := line("ak-a", "203.0.113.1:1000")
	backlog.Backlog = true
	untimed := line("ak-a", "203.0.113.1:1001")
	untimed.Written = time.Time{}
	nowhere := line("ak-a", "203.0.113.1:1002")
	nowhere.Dest = accesslog.Dest{}
	// Shadowsocks 2022 lines name no network; once bound, the connection was TCP.
	unsaid := line("ak-c", "203.0.113.1:1004")
	unsaid.Dest.Network = ""
	blocked := line("ak-b", "[2001:db8::7]:5000")
	blocked.Dest, blocked.Outbound, blocked.Blocked = accesslog.Dest{Network: "udp", Host: "10.0.0.1", Port: 53}, "blocked", true
	r.Lines([]accesslog.Line{backlog, line("ak-a", "203.0.113.1:999"), untimed, nowhere, line("operator-own", "203.0.113.1:1003"), blocked, unsaid})
	// The connection of an old line still gets its end record.
	r.Ended(ledger.Ended{Line: backlog, At: written.Add(90 * time.Second), Up: 40, Down: 2000, Requests: 3})
	r.Ended(ledger.Ended{Line: unsaid, At: written, Up: 1, Down: 2, Requests: 1})
	var out sent
	r.step(out.publish)
	if len(out) != 1 {
		t.Fatalf("sent %d reports", len(out))
	}
	records, gaps := decode(t, out[0])
	got := []string{}
	for _, rec := range records {
		b, _ := json.Marshal(rec)
		got = append(got, string(b))
	}
	want := []string{
		`{"at":"2026-10-26T12:15:07.123456Z","blocked":false,"email":"ak-a","host":"example.com","network":"tcp","outbound":"direct","port":443,"source":"203.0.113.1:999"}`,
		`{"at":"2026-10-26T12:15:07.123456Z","blocked":true,"email":"ak-b","host":"10.0.0.1","network":"udp","outbound":"blocked","port":53,"source":"[2001:db8::7]:5000"}`,
		`{"at":"2026-10-26T12:15:07.123456Z","blocked":false,"email":"ak-c","host":"example.com","network":"","outbound":"direct","port":443,"source":"203.0.113.1:1004"}`,
		`{"at":"2026-10-26T12:15:07.123456Z","blocked":false,"down":2000,"email":"ak-a","ended_at":"2026-10-26T12:16:37.123456Z","host":"example.com","network":"tcp","outbound":"direct","port":443,"requests":3,"source":"203.0.113.1:1000","up":40}`,
		`{"at":"2026-10-26T12:15:07.123456Z","blocked":false,"down":2,"email":"ak-c","ended_at":"2026-10-26T12:15:07.123456Z","host":"example.com","network":"tcp","outbound":"direct","port":443,"requests":1,"source":"203.0.113.1:1004","up":1}`,
	}
	if gaps != nil || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("records\n%s\ngaps %v", strings.Join(got, "\n"), gaps)
	}
}

// One batch at a time waits for its acknowledgement and goes again until it
// comes; a full batch is written and sent at once.
func TestBatchesWaitForTheirAcknowledgement(t *testing.T) {
	r := New(t.TempDir())
	var lines []accesslog.Line
	for i := range 2500 {
		lines = append(lines, line("ak-a", fmt.Sprintf("203.0.113.1:%d", 1000+i)))
	}
	r.Lines(lines)
	select {
	case <-r.wake:
	default:
		t.Fatal("a full batch did not wake the sender")
	}
	var out sent
	r.step(out.publish)
	r.step(out.publish)
	if len(out) != 2 || out[0].ReportID != out[1].ReportID {
		t.Fatalf("an unacknowledged batch was not sent again as it was: %d reports", len(out))
	}
	if records, _ := decode(t, out[0]); len(records) != maxBatch {
		t.Fatalf("batch of %d", len(records))
	}
	if ok, _ := r.Acknowledge("another"); ok {
		t.Fatal("settled a report it did not send")
	}
	if ok, _ := r.Acknowledge(out[0].ReportID); !ok {
		t.Fatal("not settled")
	}
	select {
	case <-r.wake:
	default:
		t.Fatal("the next full batch waits for the timer")
	}
	r.step(out.publish)
	r.Acknowledge(out[2].ReportID)
	r.step(out.publish)
	r.Acknowledge(out[3].ReportID)
	r.step(out.publish)
	records, _ := decode(t, out[3])
	if len(out) != 4 || out[2].ReportID == out[1].ReportID || len(records) != 500 || records[0]["source"] != "203.0.113.1:3000" {
		t.Fatalf("%d reports, last of %d records", len(out), len(records))
	}
}

// The queue is on disk, readable by root alone, until Cloud acknowledges it:
// a process that stops or crashes before then leaves it to the next, which
// sends it again.
func TestQueueOutlivesTheProcess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), Name)
	r := New(dir)
	r.Lines([]accesslog.Line{lineAt(0, "203.0.113.1:1", false), lineAt(1, "203.0.113.1:2", false)})
	var out sent
	r.step(out.publish)
	for path, mode := range map[string]os.FileMode{dir: os.ModeDir | 0o700, filepath.Join(dir, queueFile): 0o600, filepath.Join(dir, stateFile): 0o600} {
		if info, err := os.Stat(path); err != nil || info.Mode() != mode {
			t.Fatalf("%s: %v, %v", path, info.Mode(), err)
		}
	}
	// Restarted without an acknowledgement, and again after one.
	next := New(dir)
	next.step(out.publish)
	if records, _ := decode(t, out[1]); len(out) != 2 || sources(records) != "203.0.113.1:1 203.0.113.1:2" || out[1].ReportID == out[0].ReportID {
		t.Fatalf("after a restart %d reports: %v", len(out), records)
	}
	next.Acknowledge(out[1].ReportID)
	next.step(out.publish)
	next.spool.close()
	if info, err := os.Stat(filepath.Join(dir, queueFile)); err != nil || info.Size() != 0 || len(out) != 2 {
		t.Fatalf("an acknowledged queue holds %v bytes; %d reports", info.Size(), len(out))
	}
	last := New(dir)
	last.step(out.publish)
	if len(out) != 2 {
		t.Fatalf("an acknowledged batch went again")
	}
	// A record half written when the process died never counted.
	queue, _ := os.OpenFile(filepath.Join(dir, queueFile), os.O_WRONLY|os.O_APPEND, 0)
	queue.WriteString(`{"at":"2026-10-26T12:15:08.000000Z","email":"ak-a`)
	queue.Close()
	last.spool.close()
	torn := New(dir)
	torn.Lines([]accesslog.Line{lineAt(2, "203.0.113.1:3", false)})
	torn.step(out.publish)
	if records, _ := decode(t, out[2]); sources(records) != "203.0.113.1:3" {
		t.Fatalf("after a torn write %v", records)
	}
}

// On opening the log a restart records its lines again from the last one
// queued on, so none written while the Agent was away is lost; when the log
// no longer reaches back that far, the time between is a gap.
func TestRestartReadsTheLogAgainFromTheLastLineQueued(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	r.Lines([]accesslog.Line{lineAt(0, "203.0.113.1:1", false), lineAt(10, "203.0.113.1:2", false)})
	var out sent
	r.step(out.publish)
	r.Acknowledge(out[0].ReportID)
	r.step(out.publish)

	r = New(dir)
	r.Lines([]accesslog.Line{lineAt(0, "203.0.113.1:1", true), lineAt(10, "203.0.113.1:2", true), lineAt(20, "203.0.113.1:3", true)})
	r.Lines([]accesslog.Line{lineAt(30, "203.0.113.1:4", false)})
	r.step(out.publish)
	records, gaps := decode(t, out[1])
	if sources(records) != "203.0.113.1:2 203.0.113.1:3 203.0.113.1:4" || gaps != nil {
		t.Fatalf("read again %v, gaps %v", sources(records), gaps)
	}
	r.Acknowledge(out[1].ReportID)
	r.step(out.publish)

	// The log's tail now starts after the last line queued.
	r = New(dir)
	r.Lines([]accesslog.Line{lineAt(90, "203.0.113.1:5", true), lineAt(95, "203.0.113.1:6", false)})
	r.step(out.publish)
	records, gaps = decode(t, out[2])
	want := []gap{{StartedAt: "2026-10-26T12:15:37.123456Z", EndedAt: "2026-10-26T12:16:37.123456Z", Reason: logMissed}}
	if sources(records) != "203.0.113.1:5 203.0.113.1:6" || !reflect.DeepEqual(gaps, want) {
		t.Fatalf("read again %v, gaps %v", sources(records), gaps)
	}
	// Until Cloud has the gap it goes with every batch, also after a restart.
	r = New(dir)
	r.step(out.publish)
	if records, gaps := decode(t, out[3]); len(records) != 2 || !reflect.DeepEqual(gaps, want) {
		t.Fatalf("after a restart %v, gaps %v", records, gaps)
	}
	r.Acknowledge(out[3].ReportID)
	r.Lines([]accesslog.Line{lineAt(95, "203.0.113.1:6", true), lineAt(100, "203.0.113.1:7", false)})
	r.step(out.publish)
	if records, gaps := decode(t, out[4]); sources(records) != "203.0.113.1:6 203.0.113.1:7" || gaps != nil {
		t.Fatalf("after the gap went %v, gaps %v", sources(records), gaps)
	}
}

// Past its limit the queue drops its oldest records, the batch waiting among
// them; with the disk nearly full nothing more is written. Either way what is
// lost is a gap from the first record's time to the last, with how many.
func TestFullQueueDropsRecordsIntoAGap(t *testing.T) {
	r := New(t.TempDir())
	var out sent
	write := func(from, n int) {
		var lines []accesslog.Line
		for i := range n {
			lines = append(lines, lineAt(from+i, fmt.Sprintf("203.0.113.1:%d", 1000+from+i), false))
		}
		r.Lines(lines)
		r.step(out.publish)
	}
	write(0, 10)
	r.spool.limit = r.spool.size * 2
	write(10, 10)
	// One more goes past the limit: the oldest tenth of it goes, two records.
	write(20, 1)
	if records, _ := decode(t, out[0]); len(records) != 10 || len(out) != 3 || out[1].ReportID != out[0].ReportID {
		t.Fatalf("%d reports", len(out))
	}
	records, gaps := decode(t, out[2])
	want := []gap{{StartedAt: "2026-10-26T12:15:07.123456Z", EndedAt: "2026-10-26T12:15:08.123456Z", Reason: backlogFull, Lost: 2}}
	if len(records) != 19 || records[0]["source"] != "203.0.113.1:1002" || !reflect.DeepEqual(gaps, want) {
		t.Fatalf("%d records from %v, gaps %v", len(records), records[0]["source"], gaps)
	}
	// The batch dropped from is gone: its acknowledgement settles nothing.
	if ok, _ := r.Acknowledge(out[0].ReportID); ok {
		t.Fatal("a dropped batch was settled")
	}
	r.Acknowledge(out[2].ReportID)
	r.spool.free = func(string) (int64, error) { return minFree + 100, nil }
	write(30, 3)
	records, gaps = decode(t, out[3])
	want = []gap{{StartedAt: "2026-10-26T12:15:37.123456Z", EndedAt: "2026-10-26T12:15:39.123456Z", Reason: backlogFull, Lost: 3}}
	if len(out) != 4 || len(records) != 0 || !reflect.DeepEqual(gaps, want) {
		t.Fatalf("with the disk full %d records, gaps %v", len(records), gaps)
	}
}

// Turning the module off removes its queue.
func TestLeftoversRemoveTheQueue(t *testing.T) {
	stateDir := t.TempDir()
	r := New(Directory(stateDir))
	r.Lines([]accesslog.Line{lineAt(0, "203.0.113.1:1", false)})
	var out sent
	r.step(out.publish)
	r.spool.close()
	cleanup := Leftovers(stateDir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- cleanup.Run(ctx) }()
	<-cleanup.Done()
	cancel()
	<-done
	if _, err := os.Stat(Directory(stateDir)); !os.IsNotExist(err) {
		t.Fatalf("the queue remains: %v", err)
	}
}

// A batch of the largest records with every gap still fits one protocol
// message.
func TestFullBatchFitsOneMessage(t *testing.T) {
	r := New(t.TempDir())
	big := line("ak-"+strings.Repeat("z", 61), "[2001:db8:ffff:ffff:ffff:ffff:ffff:ffff]:65535")
	big.Dest.Host, big.Outbound = strings.Repeat("a", 255), strings.Repeat("o", maxOutbound)
	for range maxBatch {
		r.Ended(ledger.Ended{Line: big, At: written, Up: 1<<53 - 1, Down: 1<<53 - 1, Requests: 1 << 30})
	}
	for i := range maxGaps {
		r.gaps = append(r.gaps, gap{StartedAt: format(written), EndedAt: format(written), Reason: [2]string{logMissed, backlogFull}[i%2], Lost: 1 << 30})
	}
	var out sent
	r.step(out.publish)
	records, gaps := decode(t, out[0])
	message, err := protocol.Encode("report", out[0])
	if err != nil || len(message) > protocol.MaxMessage || len(records) != maxBatch || len(gaps) != maxGaps {
		t.Fatalf("message of %d bytes: %v", len(message), err)
	}
}
