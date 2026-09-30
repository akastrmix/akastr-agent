package ipqualityrunner

import (
	"context"
	"encoding/json"
	"time"

	"github.com/akastrmix/akastr-agent/internal/modules/ipqualityrunner/script"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type Handler struct {
	provider      *script.Provider
	scriptVersion string
}

func New(provider *script.Provider, scriptVersion string) *Handler {
	return &Handler{provider: provider, scriptVersion: scriptVersion}
}

func (h *Handler) CommandType() string    { return CommandType }
func (h *Handler) ExclusiveGroup() string { return "ipquality-runner" }
func (h *Handler) Accepting() bool        { return true }

func (h *Handler) Validate(payload json.RawMessage) error {
	_, err := DecodePayload(payload)
	return err
}

func (h *Handler) Recover(protocol.OperationOffer) protocol.ExecutionResult {
	return h.failure("interrupted_unknown", "", "", "")
}

func (h *Handler) Run(ctx context.Context, offer protocol.OperationOffer) protocol.ExecutionResult {
	payload, err := DecodePayload(offer.Payload)
	if err != nil {
		return h.failure("payload_invalid", "", "", "")
	}
	run := h.provider.Run(ctx, script.Request{
		ProxyPort:    payload.ProxyPort,
		Credentials:  script.Profile{Username: payload.ProxyUsername, Password: payload.ProxyPassword},
		ExpectedIPv4: payload.ExpectedIPv4,
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
