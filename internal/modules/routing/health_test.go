package routing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/akastrmix/akastr-agent/internal/panel"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type healthPanel struct {
	outbounds string
	answers   map[string]panel.OutboundTest
	tested    []string
	all       []string
}

func (p *healthPanel) Template(context.Context) (panel.Template, error) {
	return panel.Template{Config: map[string]json.RawMessage{"outbounds": json.RawMessage(p.outbounds)}}, nil
}

func (p *healthPanel) TestOutbound(_ context.Context, outbound, all json.RawMessage) (panel.OutboundTest, error) {
	var o struct {
		Tag string `json:"tag"`
	}
	_ = json.Unmarshal(outbound, &o)
	p.tested, p.all = append(p.tested, o.Tag), append(p.all, string(all))
	if o.Tag == "ak-o-gone" {
		return panel.OutboundTest{}, errors.New("xui_request_unknown")
	}
	return p.answers[o.Tag], nil
}

// Only a request that went out through the outbound and failed counts as
// down: a false "down" alerts the owner, a false "up" hides a broken exit.
// The panel's own words can name the outbound's server and never leave.
func TestHealthReportsEachOwnedOutboundWithoutPanelText(t *testing.T) {
	p := &healthPanel{
		outbounds: `[{"tag":"direct","protocol":"freedom"},{"tag":"blocked","protocol":"blackhole"},` +
			`{"tag":"ak-o-up","protocol":"shadowsocks"},{"tag":"ak-o-down","protocol":"vless"},` +
			`{"tag":"ak-o-busy","protocol":"vless"},{"tag":"ak-o-gone","protocol":"vless"},{"tag":"operator","protocol":"vless"}]`,
		answers: map[string]panel.OutboundTest{
			"ak-o-up":   {Success: true, Delay: 182},
			"ak-o-down": {Error: "Request failed: Get \"https://www.google.com/generate_204\": dial exit.example.net:443: i/o timeout"},
			"ak-o-busy": {Error: "Another outbound test is already running, please wait"},
		},
	}
	h := NewHealth(p)
	var sent []protocol.ReportBody
	h.round(context.Background(), func(r protocol.ReportBody) error { sent = append(sent, r); return nil })

	if strings.Join(p.tested, ",") != "ak-o-up,ak-o-down,ak-o-busy,ak-o-gone" || p.all[0] != p.outbounds {
		t.Fatalf("tested %v with %v", p.tested, p.all)
	}
	if len(sent) != 1 || sent[0].Kind != KindHealth {
		t.Fatalf("sent %+v", sent)
	}
	want := `{"outbounds":[{"tag":"ak-o-up","up":true,"delay_ms":182},{"tag":"ak-o-down","up":false,"delay_ms":null},` +
		`{"tag":"ak-o-busy","up":null,"delay_ms":null},{"tag":"ak-o-gone","up":null,"delay_ms":null}]}`
	if data, _ := sent[0].Data.(json.RawMessage); string(data) != want {
		t.Fatalf("data %s", sent[0].Data)
	}

	// A ready session gets the same report again until Cloud acknowledges it.
	h.ControlReady()
	<-h.wake
	h.send(func(r protocol.ReportBody) error { sent = append(sent, r); return nil })
	if len(sent) != 2 || sent[1].ReportID != sent[0].ReportID {
		t.Fatalf("resent %+v", sent)
	}
	if ok, _ := h.Acknowledge(sent[0].ReportID); !ok {
		t.Fatal("acknowledge")
	}
	h.send(func(r protocol.ReportBody) error { sent = append(sent, r); return nil })
	if len(sent) != 2 {
		t.Fatal("an acknowledged report was sent again")
	}
}
