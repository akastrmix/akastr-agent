package changeip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/akastrmix/akastr-agent/internal/features/ipwatch"
	"github.com/akastrmix/akastr-agent/internal/operation"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	changeprovider "github.com/akastrmix/akastr-agent/internal/providers/changeip"
)

type Handler struct {
	engine         *operation.Engine
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

func New(engine *operation.Engine, observer ipwatch.AddressObserver, provider changeprovider.Provider, reconciler changeReconciler, observeTimeout time.Duration) *Handler {
	return &Handler{
		engine: engine, observer: observer, provider: provider, reconciler: reconciler,
		observeTimeout: observeTimeout,
	}
}

func (h *Handler) Execute(ctx context.Context, offer protocol.OperationOffer) (protocol.ExecutionResult, error) {
	if recent, found := h.engine.Recent(offer.CommandID); found && len(recent.TerminalResult) > 0 {
		var result protocol.ExecutionResult
		if json.Unmarshal(recent.TerminalResult, &result) == nil {
			return result, nil
		}
		return protocol.ExecutionResult{}, errors.New("decode persisted ChangeIP terminal result")
	}
	if _, err := h.engine.Begin(offer.CommandID, "changeip.execute", "target-network"); err != nil {
		if _, active := h.engine.Active(offer.CommandID); active {
			address := offer.ChangeIP.ExpectedIPv4
			if h.reconciler != nil {
				if persistedAddress, found := h.reconciler.ChangeAddress(offer.CommandID); found {
					address = persistedAddress
				}
			}
			result := reconciliationPending(&address, time.Now().UTC())
			persisted, _ := json.Marshal(result)
			if _, finishError := h.engine.FinishWithResult(
				offer.CommandID, operation.StatusSucceeded, result.Code, persisted,
			); finishError == nil {
				return result, nil
			} else {
				return protocol.ExecutionResult{}, fmt.Errorf("persist recovered ChangeIP terminal result: %w", finishError)
			}
		}
		return protocol.ExecutionResult{}, fmt.Errorf("begin ChangeIP operation: %w", err)
	}
	result := h.execute(ctx, offer)
	persisted, _ := json.Marshal(result)
	status := operation.StatusFailed
	if result.Outcome == "succeeded" {
		status = operation.StatusSucceeded
	} else if result.Outcome == "cancelled" {
		status = operation.StatusCancelled
	}
	if _, err := h.engine.FinishWithResult(offer.CommandID, status, result.Code, persisted); err != nil {
		return protocol.ExecutionResult{}, fmt.Errorf("persist ChangeIP terminal result: %w", err)
	}
	return result, nil
}

func (h *Handler) execute(ctx context.Context, offer protocol.OperationOffer) protocol.ExecutionResult {
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
