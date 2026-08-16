package changeip

import (
	"context"
	"encoding/json"
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
	calls int
}

func (o *fakeObserver) Observe(context.Context, ipwatch.Family) (ipwatch.Observation, error) {
	o.calls++
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

func TestCompletedProviderReturnsTriggeredWithoutASecondObservation(t *testing.T) {
	observer := &fakeObserver{steps: []observerStep{{address: "8.8.8.8"}}}
	handler := &Handler{
		observer: observer, provider: fakeProvider{}, observeTimeout: time.Second,
	}
	result := handler.execute(context.Background(), offerFor("8.8.8.8"))
	oldIPv4, oldOk := result.Result["old_ipv4"].(*string)
	newIPv4, newOk := result.Result["new_ipv4"].(*string)
	if result.Outcome != "succeeded" || result.Code != "change_triggered" ||
		!oldOk || oldIPv4 == nil || *oldIPv4 != "8.8.8.8" || !newOk || newIPv4 != nil {
		t.Fatalf("execute() = %#v", result)
	}
	if observer.calls != 1 {
		t.Fatalf("observer calls = %d, want exactly one preflight observation", observer.calls)
	}
}

func offerFor(expectedIPv4 string) protocol.OperationOffer {
	payload, _ := json.Marshal(protocol.ChangeIPPayload{ExpectedIPv4: expectedIPv4})
	return protocol.OperationOffer{Payload: payload}
}
