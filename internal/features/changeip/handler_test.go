package changeip

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/features/ipwatch"
	"github.com/akastrmix/akastr-agent/internal/operation"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	changeprovider "github.com/akastrmix/akastr-agent/internal/providers/changeip"
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

type fakeProvider struct{ result changeprovider.Result }

func (p fakeProvider) Run(context.Context) changeprovider.Result {
	return p.result
}

type fakeReconciler struct {
	commandID string
	address   string
}

func (r *fakeReconciler) ArmChange(commandID, address string, _ time.Time) error {
	r.commandID, r.address = commandID, address
	return nil
}
func (r *fakeReconciler) CancelChange(commandID string) error {
	if commandID == r.commandID {
		r.commandID, r.address = "", ""
	}
	return nil
}
func (r *fakeReconciler) HasChange(commandID string) bool { return commandID == r.commandID }
func (r *fakeReconciler) ChangeAddress(commandID string) (string, bool) {
	return r.address, commandID == r.commandID
}

func TestCompletedProviderReturnsTriggeredWithoutASecondObservation(t *testing.T) {
	observer := &fakeObserver{steps: []observerStep{{address: "8.8.8.8"}}}
	handler := &Handler{
		observer: observer,
		provider: fakeProvider{result: changeprovider.Result{
			State: changeprovider.TriggerConfirmed, Code: "completed", FinishedAt: time.Now().UTC(),
		}},
		reconciler: &fakeReconciler{}, observeTimeout: time.Second,
	}
	result := handler.execute(context.Background(), offerFor("8.8.8.8"))
	oldIPv4, oldOk := result.Result["old_ipv4"].(*string)
	if result.Outcome != "succeeded" || result.Code != "change_triggered" ||
		!oldOk || oldIPv4 == nil || *oldIPv4 != "8.8.8.8" || len(result.Result) != 2 {
		t.Fatalf("execute() = %#v", result)
	}
	if observer.calls != 1 {
		t.Fatalf("observer calls = %d, want exactly one preflight observation", observer.calls)
	}
}

func TestProviderOutcomeControlsReconciliationWithoutRetry(t *testing.T) {
	tests := []struct {
		name          string
		providerState changeprovider.TriggerState
		providerCode  string
		wantOutcome   string
		wantCode      string
		wantArmed     bool
	}{
		{
			name: "unknown remains armed", providerState: changeprovider.TriggerUnknown,
			providerCode: "trigger_outcome_unknown", wantOutcome: "succeeded",
			wantCode: "change_trigger_unknown", wantArmed: true,
		},
		{
			name: "definite failure cancels", providerState: changeprovider.TriggerFailed,
			providerCode: "exited_nonzero", wantOutcome: "failed",
			wantCode: "exited_nonzero", wantArmed: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reconciler := &fakeReconciler{}
			handler := &Handler{
				observer: &fakeObserver{steps: []observerStep{{address: "8.8.8.8"}}},
				provider: fakeProvider{result: changeprovider.Result{
					State: test.providerState, Code: test.providerCode, FinishedAt: time.Now().UTC(),
				}},
				reconciler: reconciler, observeTimeout: time.Second,
			}
			result := handler.execute(context.Background(), offerFor("8.8.8.8"))
			if result.Outcome != test.wantOutcome || result.Code != test.wantCode {
				t.Fatalf("execute() = %#v", result)
			}
			if armed := reconciler.commandID != ""; armed != test.wantArmed {
				t.Fatalf("reconciliation armed = %t, want %t", armed, test.wantArmed)
			}
		})
	}
}

func TestActiveJournalRecoveryNeverRunsProviderAgain(t *testing.T) {
	engine, err := operation.Open(operation.Options{
		StateFile: filepath.Join(t.TempDir(), "operations.json"), RecentLimit: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	offer := offerFor("8.8.8.8")
	if _, err := engine.Begin(offer.CommandID, "changeip.execute", "target-network"); err != nil {
		t.Fatal(err)
	}
	provider := &countingProvider{}
	handler := New(engine, nil, provider, nil, time.Second)
	result, err := handler.Execute(t.Context(), offer)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "failed" || result.Code != "interrupted_unknown" {
		t.Fatalf("Execute() = %#v", result)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", provider.calls)
	}
	if _, active := engine.Active(offer.CommandID); active {
		t.Fatal("active journal was not released")
	}
}

type countingProvider struct{ calls int }

func (p *countingProvider) Run(context.Context) changeprovider.Result {
	p.calls++
	return changeprovider.Result{}
}

func offerFor(expectedIPv4 string) protocol.OperationOffer {
	return protocol.OperationOffer{
		CommandID: "123e4567-e89b-42d3-a456-426614174000",
		ChangeIP:  &protocol.ChangeIPPayload{ExpectedIPv4: expectedIPv4},
	}
}
