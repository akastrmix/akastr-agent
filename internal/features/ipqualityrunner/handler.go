package ipqualityrunner

import (
	"context"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/providers/ipquality/script"
)

type Handler struct {
	provider      *script.Provider
	scriptVersion string
}

func New(provider *script.Provider, scriptVersion string) *Handler {
	return &Handler{provider: provider, scriptVersion: scriptVersion}
}

func (h *Handler) Recover(protocol.OperationOffer) protocol.ExecutionResult {
	return h.failure("interrupted_unknown", "", "", "")
}

func (h *Handler) Run(ctx context.Context, offer protocol.OperationOffer) protocol.ExecutionResult {
	payload := *offer.IPQuality
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
