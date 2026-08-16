package changeip

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/akastrmix/akastr-agent/internal/features/ipwatch"
	"github.com/akastrmix/akastr-agent/internal/operation"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	changecommand "github.com/akastrmix/akastr-agent/internal/providers/changeip/command"
)

type Handler struct {
	engine         *operation.Engine
	observer       ipwatch.AddressObserver
	provider       commandProvider
	observeTimeout time.Duration
}

type commandProvider interface {
	Run(context.Context) changecommand.Result
}

func New(engine *operation.Engine, observer ipwatch.AddressObserver, provider commandProvider, observeTimeout time.Duration) *Handler {
	return &Handler{
		engine: engine, observer: observer, provider: provider,
		observeTimeout: observeTimeout,
	}
}

func (h *Handler) Execute(ctx context.Context, offer protocol.OperationOffer) protocol.ExecutionResult {
	if recent, found := h.engine.Recent(offer.CommandID); found && len(recent.TerminalResult) > 0 {
		var result protocol.ExecutionResult
		if json.Unmarshal(recent.TerminalResult, &result) == nil {
			return result
		}
	}
	if _, err := h.engine.Begin(offer.CommandID, "changeip.execute", "target-network"); err != nil {
		if _, active := h.engine.Active(offer.CommandID); active {
			result := failure("interrupted_unknown", nil, nil, time.Now().UTC())
			persisted, _ := json.Marshal(result)
			if _, finishError := h.engine.FinishWithResult(
				offer.CommandID, operation.StatusFailed, result.Code, persisted,
			); finishError == nil {
				return result
			}
		}
		code := "local_conflict"
		if errors.Is(err, operation.ErrDuplicate) {
			code = "result_recovery_failed"
		}
		return failure(code, nil, nil, time.Now().UTC())
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
		return failure("state_persist_failed", nil, nil, time.Now().UTC())
	}
	return result
}

func (h *Handler) execute(ctx context.Context, offer protocol.OperationOffer) protocol.ExecutionResult {
	var payload protocol.ChangeIPPayload
	if err := decodeExact(offer.Payload, &payload); err != nil {
		return failure("payload_invalid", nil, nil, time.Now().UTC())
	}
	observeContext, cancelObserve := context.WithTimeout(ctx, h.observeTimeout)
	beforeObservation, err := h.observer.Observe(observeContext, ipwatch.IPv4)
	cancelObserve()
	if err != nil {
		return failure("ipv4_observe_failed", nil, nil, time.Now().UTC())
	}
	before := beforeObservation.Address.String()
	if before != payload.ExpectedIPv4 {
		return failure("stale_expected_ipv4", &before, &before, beforeObservation.ObservedAt)
	}
	providerResult := h.provider.Run(ctx)
	if providerResult.Code != changecommand.CodeCompleted {
		outcome := "failed"
		if providerResult.Code == changecommand.CodeCancelled {
			outcome = "cancelled"
		}
		result := failure(providerResult.Code, &before, &before, providerResult.FinishedAt)
		result.Outcome = outcome
		return result
	}
	return protocol.ExecutionResult{
		Outcome: "succeeded", Code: "change_triggered",
		Result: changeResult(&before, nil, providerResult.FinishedAt),
	}
}

func failure(code string, oldIPv4, newIPv4 *string, observedAt time.Time) protocol.ExecutionResult {
	return protocol.ExecutionResult{
		Outcome: "failed", Code: code, Result: changeResult(oldIPv4, newIPv4, observedAt),
	}
}

func changeResult(oldIPv4, newIPv4 *string, observedAt time.Time) map[string]any {
	return map[string]any{
		"old_ipv4": oldIPv4, "new_ipv4": newIPv4,
		"observed_at": observedAt.UTC().Format(time.RFC3339Nano),
	}
}

func decodeExact(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("payload contains trailing JSON")
	}
	return nil
}
