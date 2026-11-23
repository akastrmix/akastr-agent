package changeip

import (
	"context"
	"encoding/json"
	"net/netip"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

// ObserveIPv4 observes the node's public IPv4 address and when it was seen.
type ObserveIPv4 func(context.Context) (netip.Addr, time.Time, error)

type Handler struct {
	observe        ObserveIPv4
	provider       Provider
	reconciler     changeReconciler
	observeTimeout time.Duration
}

type changeReconciler interface {
	AddressSettled() bool
	ArmChange(commandID, address string, startedAt time.Time) error
	CancelChange(commandID string) error
	ChangeAddress(commandID string) (string, bool)
}

func New(observe ObserveIPv4, provider Provider, reconciler changeReconciler, observeTimeout time.Duration) *Handler {
	return &Handler{
		observe: observe, provider: provider, reconciler: reconciler,
		observeTimeout: observeTimeout,
	}
}

func (h *Handler) CommandType() string    { return CommandType }
func (h *Handler) ExclusiveGroup() string { return "target-network" }

func (h *Handler) Validate(payload json.RawMessage) error {
	_, err := DecodePayload(payload)
	return err
}

// Accepting waits until Cloud has stored this node's current IPv4 address, so
// the address a command expects is the one the node also sees.
func (h *Handler) Accepting() bool {
	return h.reconciler != nil && h.reconciler.AddressSettled()
}

func (h *Handler) Recover(offer protocol.OperationOffer) protocol.ExecutionResult {
	payload, _ := DecodePayload(offer.Payload)
	address := payload.ExpectedIPv4
	if h.reconciler != nil {
		if persisted, found := h.reconciler.ChangeAddress(offer.CommandID); found {
			address = persisted
		}
	}
	return reconciliationPending(&address, time.Now().UTC())
}

func (h *Handler) Failed(_ protocol.OperationOffer, code string) protocol.ExecutionResult {
	return failure(code, nil, time.Now().UTC())
}

func (h *Handler) Run(ctx context.Context, offer protocol.OperationOffer) protocol.ExecutionResult {
	payload, err := DecodePayload(offer.Payload)
	if err != nil {
		return failure("payload_invalid", nil, time.Now().UTC())
	}
	observeContext, cancelObserve := context.WithTimeout(ctx, h.observeTimeout)
	beforeAddress, observedAt, err := h.observe(observeContext)
	cancelObserve()
	if err != nil {
		return failure("ipv4_observe_failed", nil, time.Now().UTC())
	}
	before := beforeAddress.String()
	if before != payload.ExpectedIPv4 {
		return failure("stale_expected_ipv4", &before, observedAt)
	}
	if h.reconciler == nil || h.reconciler.ArmChange(offer.CommandID, before, time.Now().UTC()) != nil {
		return failure("reconciliation_state_failed", &before, time.Now().UTC())
	}
	providerResult := h.provider.Run(ctx)
	if providerResult.State == TriggerFailed {
		if err := h.reconciler.CancelChange(offer.CommandID); err != nil {
			return reconciliationPending(&before, providerResult.FinishedAt)
		}
		result := failure(providerResult.Code, &before, providerResult.FinishedAt)
		if providerResult.Reply != "" {
			result.Result["provider_reply"] = providerResult.Reply
		}
		return result
	}
	if providerResult.State == TriggerUnknown {
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
