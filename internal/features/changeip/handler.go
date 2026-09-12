package changeip

import (
	"context"
	"time"

	"github.com/akastrmix/akastr-agent/internal/features/ipwatch"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	changeprovider "github.com/akastrmix/akastr-agent/internal/providers/changeip"
)

type Handler struct {
	observer       ipwatch.AddressObserver
	provider       changeprovider.Provider
	reconciler     changeReconciler
	observeTimeout time.Duration
}

type changeReconciler interface {
	ArmChange(commandID, address string, startedAt time.Time) error
	CancelChange(commandID string) error
	ChangeAddress(commandID string) (string, bool)
}

func New(observer ipwatch.AddressObserver, provider changeprovider.Provider, reconciler changeReconciler, observeTimeout time.Duration) *Handler {
	return &Handler{
		observer: observer, provider: provider, reconciler: reconciler,
		observeTimeout: observeTimeout,
	}
}

func (h *Handler) Recover(offer protocol.OperationOffer) protocol.ExecutionResult {
	address := offer.ChangeIP.ExpectedIPv4
	if h.reconciler != nil {
		if persisted, found := h.reconciler.ChangeAddress(offer.CommandID); found {
			address = persisted
		}
	}
	return reconciliationPending(&address, time.Now().UTC())
}

func (h *Handler) Run(ctx context.Context, offer protocol.OperationOffer) protocol.ExecutionResult {
	payload := *offer.ChangeIP
	observeContext, cancelObserve := context.WithTimeout(ctx, h.observeTimeout)
	beforeObservation, err := h.observer.Observe(observeContext, ipwatch.IPv4)
	cancelObserve()
	if err != nil {
		return failure("ipv4_observe_failed", nil, time.Now().UTC())
	}
	before := beforeObservation.Address.String()
	if before != payload.ExpectedIPv4 {
		return failure("stale_expected_ipv4", &before, beforeObservation.ObservedAt)
	}
	if h.reconciler == nil || h.reconciler.ArmChange(offer.CommandID, before, time.Now().UTC()) != nil {
		return failure("reconciliation_state_failed", &before, time.Now().UTC())
	}
	providerResult := h.provider.Run(ctx)
	if providerResult.State == changeprovider.TriggerFailed {
		if err := h.reconciler.CancelChange(offer.CommandID); err != nil {
			return reconciliationPending(&before, providerResult.FinishedAt)
		}
		return failure(providerResult.Code, &before, providerResult.FinishedAt)
	}
	if providerResult.State == changeprovider.TriggerUnknown {
		return reconciliationPending(&before, providerResult.FinishedAt)
	}
	return protocol.ExecutionResult{
		Outcome: "succeeded", Code: "change_triggered",
		Result: changeResult(&before, providerResult.FinishedAt),
	}
}

func reconciliationPending(oldIPv4 *string, observedAt time.Time) protocol.ExecutionResult {
	return protocol.ExecutionResult{
		Outcome: "succeeded", Code: "change_trigger_unknown",
		Result: changeResult(oldIPv4, observedAt),
	}
}

func failure(code string, oldIPv4 *string, observedAt time.Time) protocol.ExecutionResult {
	return protocol.ExecutionResult{
		Outcome: "failed", Code: code, Result: changeResult(oldIPv4, observedAt),
	}
}

func changeResult(oldIPv4 *string, observedAt time.Time) map[string]any {
	return map[string]any{
		"old_ipv4": oldIPv4, "observed_at": observedAt.UTC().Format(time.RFC3339Nano),
	}
}
