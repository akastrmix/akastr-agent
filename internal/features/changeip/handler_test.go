package changeip

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/features/ipwatch"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	changecommand "github.com/akastrmix/akastr-agent/internal/providers/changeip/command"
)

type observerStep struct {
	address string
	err     error
}

type fakeObserver struct {
	steps []observerStep
	index int
}

func (o *fakeObserver) Observe(context.Context, ipwatch.Family) (ipwatch.Observation, error) {
	index := o.index
	if index < len(o.steps)-1 {
		o.index++
	}
	step := o.steps[index]
	if step.err != nil {
		return ipwatch.Observation{}, step.err
	}
	return ipwatch.Observation{
		Address: netip.MustParseAddr(step.address), ObservedAt: time.Now().UTC(),
	}, nil
}

type fakeProvider struct{}

func (fakeProvider) Run(context.Context) changecommand.Result {
	return changecommand.Result{Code: changecommand.CodeCompleted, FinishedAt: time.Now().UTC()}
}

func TestPostChangeObservationTimeoutIsNotReportedAsUnchanged(t *testing.T) {
	handler := &Handler{
		observer: &fakeObserver{steps: []observerStep{
			{address: "8.8.8.8"}, {err: errors.New("network unavailable")},
		}},
		provider: fakeProvider{}, observeTimeout: 3 * time.Millisecond, pollInterval: time.Millisecond,
	}
	result := handler.execute(context.Background(), offerFor("8.8.8.8"))
	encoded, _ := json.Marshal(result.Result)
	if result.Code != "ipv4_observe_timed_out" || !bytes.Contains(encoded, []byte(`"new_ipv4":null`)) {
		t.Fatalf("execute() = %#v", result)
	}
}

func TestPostChangeObservedOldAddressIsReportedAsUnchanged(t *testing.T) {
	handler := &Handler{
		observer: &fakeObserver{steps: []observerStep{{address: "8.8.8.8"}}},
		provider: fakeProvider{}, observeTimeout: 3 * time.Millisecond, pollInterval: time.Millisecond,
	}
	result := handler.execute(context.Background(), offerFor("8.8.8.8"))
	if result.Code != "ipv4_unchanged" {
		t.Fatalf("execute() = %#v", result)
	}
}

func offerFor(expectedIPv4 string) protocol.OperationOffer {
	payload, _ := json.Marshal(protocol.ChangeIPPayload{ExpectedIPv4: expectedIPv4})
	return protocol.OperationOffer{Payload: payload}
}
