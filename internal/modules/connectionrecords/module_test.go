package connectionrecords

import (
	"encoding/json"
	"fmt"
	"net/netip"
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

// sent collects what the module publishes.
type sent []protocol.ReportBody

func (s *sent) publish(r protocol.ReportBody) error { *s = append(*s, r); return nil }

// decode reads a batch the way Cloud does: every field of a record present,
// the four of an end record all or none.
func decode(t *testing.T, r protocol.ReportBody) (records []map[string]any, dropped int) {
	t.Helper()
	if r.Kind != KindBatch {
		t.Fatalf("kind %q", r.Kind)
	}
	var b struct {
		Records []map[string]any `json:"records"`
		Dropped int              `json:"dropped"`
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
	return b.Records, b.Dropped
}

// A record is made of each new line of an owned client that says when and
// where; an end record carries the same line with what its connection did.
func TestRecordsOfLinesAndEndedConnections(t *testing.T) {
	r := New()
	backlog := line("ak-a", "203.0.113.1:1000")
	backlog.Backlog = true
	untimed := line("ak-a", "203.0.113.1:1001")
	untimed.Written = time.Time{}
	nowhere := line("ak-a", "203.0.113.1:1002")
	nowhere.Dest = accesslog.Dest{}
	blocked := line("ak-b", "[2001:db8::7]:5000")
	blocked.Dest, blocked.Outbound, blocked.Blocked = accesslog.Dest{Network: "udp", Host: "10.0.0.1", Port: 53}, "blocked", true
	r.Lines([]accesslog.Line{line("ak-a", "203.0.113.1:999"), backlog, untimed, nowhere, line("operator-own", "203.0.113.1:1003"), blocked})
	// The connection of an old line still gets its end record.
	r.Ended(ledger.Ended{Line: backlog, At: written.Add(90 * time.Second), Up: 40, Down: 2000, Requests: 3})
	var out sent
	r.send(out.publish)
	if len(out) != 1 {
		t.Fatalf("sent %d reports", len(out))
	}
	records, dropped := decode(t, out[0])
	got := []string{}
	for _, rec := range records {
		b, _ := json.Marshal(rec)
		got = append(got, string(b))
	}
	want := []string{
		`{"at":"2026-10-26T12:15:07.123456Z","blocked":false,"email":"ak-a","host":"example.com","network":"tcp","outbound":"direct","port":443,"source":"203.0.113.1:999"}`,
		`{"at":"2026-10-26T12:15:07.123456Z","blocked":true,"email":"ak-b","host":"10.0.0.1","network":"udp","outbound":"blocked","port":53,"source":"[2001:db8::7]:5000"}`,
		`{"at":"2026-10-26T12:15:07.123456Z","blocked":false,"down":2000,"email":"ak-a","ended_at":"2026-10-26T12:16:37.123456Z","host":"example.com","network":"tcp","outbound":"direct","port":443,"requests":3,"source":"203.0.113.1:1000","up":40}`,
	}
	if dropped != 0 || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("records\n%s\ndropped %d", strings.Join(got, "\n"), dropped)
	}
}

// One batch at a time waits for its acknowledgement and goes again until it
// comes; a full queue sends the next batch at once.
func TestBatchesWaitForTheirAcknowledgement(t *testing.T) {
	r := New()
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
	r.send(out.publish)
	r.send(out.publish)
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
	r.send(out.publish)
	r.Acknowledge(out[2].ReportID)
	r.send(out.publish)
	r.Acknowledge(out[3].ReportID)
	r.send(out.publish)
	if records, _ := decode(t, out[3]); len(out) != 4 || out[2].ReportID == out[1].ReportID || len(records) != 500 {
		t.Fatalf("%d reports, last of %d records", len(out), len(records))
	}
}

// Beyond what memory holds the oldest records go, and the next batch says
// how many.
func TestBacklogDropsTheOldest(t *testing.T) {
	r := New()
	for i := range maxQueued + 1 {
		r.Lines([]accesslog.Line{line("ak-a", fmt.Sprintf("203.0.113.%d:%d", 1+i/60000, 1000+i%60000))})
	}
	var out sent
	r.send(out.publish)
	records, dropped := decode(t, out[0])
	if dropped != maxQueued/10+1 || records[0]["source"] != fmt.Sprintf("203.0.113.1:%d", 1000+dropped) {
		t.Fatalf("dropped %d, first %v", dropped, records[0]["source"])
	}
	r.Acknowledge(out[0].ReportID)
	r.send(out.publish)
	if _, dropped := decode(t, out[1]); dropped != 0 {
		t.Fatalf("dropped %d again", dropped)
	}
}

// A batch of the largest records still fits one protocol message.
func TestFullBatchFitsOneMessage(t *testing.T) {
	r := New()
	big := line("ak-"+strings.Repeat("z", 61), "[2001:db8:ffff:ffff:ffff:ffff:ffff:ffff]:65535")
	big.Dest.Host, big.Outbound = strings.Repeat("a", 255), strings.Repeat("o", maxOutbound)
	for range maxBatch {
		r.Ended(ledger.Ended{Line: big, At: written, Up: 1<<53 - 1, Down: 1<<53 - 1, Requests: 1 << 30})
	}
	var out sent
	r.send(out.publish)
	message, err := protocol.Encode("report", out[0])
	if err != nil || len(message) > protocol.MaxMessage {
		t.Fatalf("message of %d bytes: %v", len(message), err)
	}
}
