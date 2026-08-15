package ipqualityrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/akastrmix/akastr-agent/internal/operation"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/providers/ipquality/script"
)

type Handler struct {
	engine        *operation.Engine
	provider      *script.Provider
	scriptVersion string
}

func New(engine *operation.Engine, provider *script.Provider, scriptVersion string) *Handler {
	return &Handler{engine: engine, provider: provider, scriptVersion: scriptVersion}
}

func (h *Handler) Execute(ctx context.Context, offer protocol.OperationOffer) protocol.ExecutionResult {
	if recent, found := h.engine.Recent(offer.CommandID); found && len(recent.TerminalResult) > 0 {
		var result protocol.ExecutionResult
		if json.Unmarshal(recent.TerminalResult, &result) == nil {
			return result
		}
	}
	if _, err := h.engine.Begin(offer.CommandID, "ipquality.execute", "ipquality-runner"); err != nil {
		if _, active := h.engine.Active(offer.CommandID); active {
			result := h.failure("interrupted_unknown", "", "", "")
			persisted, _ := json.Marshal(result)
			if _, finishError := h.engine.FinishWithResult(
				offer.CommandID, operation.StatusFailed, result.Code, persisted,
			); finishError == nil {
				return result
			}
		}
		code := "runner_busy"
		if errors.Is(err, operation.ErrDuplicate) {
			code = "result_recovery_failed"
		}
		return h.failure(code, "", "", "")
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
		return h.failure("state_persist_failed", "", "", "")
	}
	return result
}

func (h *Handler) execute(ctx context.Context, offer protocol.OperationOffer) protocol.ExecutionResult {
	var payload protocol.IPQualityPayload
	decoder := json.NewDecoder(bytes.NewReader(offer.Payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil {
		return h.failure("payload_invalid", "", "", "")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return h.failure("payload_invalid", "", "", "")
	}
	if payload.ScriptVersion != h.scriptVersion {
		return h.failure("script_version_mismatch", "", "", "")
	}
	run := h.provider.Run(ctx, script.Request{
		ProxyPort:      payload.ProxyPort,
		ProxyProfileID: payload.ProxyProfileID, ExpectedIPv4: payload.ExpectedIPv4,
	})
	result := map[string]any{
		"report_url":        nullable(run.ReportURL),
		"proxy_ipv4_before": nullable(run.IPv4Before),
		"proxy_ipv4_after":  nullable(run.IPv4After),
		"script_version":    h.scriptVersion,
		"checked_at":        run.CheckedAt.UTC().Format(time.RFC3339Nano),
	}
	outcome := "failed"
	if run.Code == "report_ready" {
		outcome = "succeeded"
	} else if run.Code == "cancelled" {
		outcome = "cancelled"
	}
	return protocol.ExecutionResult{Outcome: outcome, Code: run.Code, Result: result}
}

func (h *Handler) failure(code, reportURL, before, after string) protocol.ExecutionResult {
	return protocol.ExecutionResult{
		Outcome: "failed", Code: code,
		Result: map[string]any{
			"report_url":        nullable(reportURL),
			"proxy_ipv4_before": nullable(before),
			"proxy_ipv4_after":  nullable(after),
			"script_version":    h.scriptVersion,
			"checked_at":        time.Now().UTC().Format(time.RFC3339Nano),
		},
	}
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
