package xui

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/xray/ledger"
)

// report sends one usage report of u and decodes it as Cloud does.
func report(t *testing.T, u *Usage) (protocol.ReportBody, usageReport) {
	t.Helper()
	var sent []protocol.ReportBody
	u.report(func(r protocol.ReportBody) error { sent = append(sent, r); return nil })
	if len(sent) != 1 || sent[0].Kind != "xui.usage" {
		t.Fatalf("sent %v", sent)
	}
	// The report goes out as JSON inside the envelope, not as encoded bytes.
	raw, ok := sent[0].Data.(json.RawMessage)
	if !ok {
		t.Fatalf("report data is %T", sent[0].Data)
	}
	r, err := protocol.DecodeStrict[usageReport](raw, "xui.usage", "epoch", "error_code", "clients")
	if err != nil {
		t.Fatal(err)
	}
	return sent[0], r
}

// Cloud must know when the counts may be short or too many to send, so it
// does not judge on them; only owned clients are reported.
func TestUsageReportSaysWhenCountsMayBeShort(t *testing.T) {
	usage := ledger.Usage{Epoch: "e1", Users: []ledger.Total{{Email: "ak-a", Up: 5, Down: 100, Connections: 1}, {Email: "operator-own", Down: 7}}}
	u := NewUsage(func() ledger.Usage { return usage })
	if _, r := report(t, u); r.Epoch != "e1" || r.ErrorCode != "" || len(r.Clients) != 1 ||
		r.Clients[0] != (usageClient{Email: "ak-a", Up: 5, Down: 100, Connections: 1}) {
		t.Fatalf("healthy report %+v", r)
	}
	usage.Log = errors.New("the Xray access log is off")
	if _, r := report(t, u); r.ErrorCode != "xui_log_unavailable" || len(r.Clients) != 1 {
		t.Fatalf("log off reported %+v", r)
	}
	usage.Kernel = errors.New("sock_diag: permission denied")
	if _, r := report(t, u); r.ErrorCode != "xui_usage_unavailable" {
		t.Fatalf("kernel unavailable reported %q", r.ErrorCode)
	}
	usage.Kernel, usage.Log, usage.Users = nil, nil, nil
	for i := 0; i <= maxUsageClients; i++ {
		usage.Users = append(usage.Users, ledger.Total{Email: fmt.Sprintf("ak-c%d", i), Down: 1})
	}
	if _, r := report(t, u); r.ErrorCode != "xui_usage_unavailable" || len(r.Clients) != 0 || r.Epoch != "e1" {
		t.Fatalf("report %s with %d clients in epoch %s", r.ErrorCode, len(r.Clients), r.Epoch)
	}
}

// Only the newest report waits for its acknowledgement.
func TestUsageAcknowledgeSettlesTheNewestReport(t *testing.T) {
	u := NewUsage(func() ledger.Usage { return ledger.Usage{Epoch: "e1"} })
	older, _ := report(t, u)
	newer, _ := report(t, u)
	if ok, _ := u.Acknowledge(older.ReportID); ok {
		t.Fatal("a replaced report was settled")
	}
	if ok, _ := u.Acknowledge(newer.ReportID); !ok {
		t.Fatal("the newest report was not settled")
	}
}
