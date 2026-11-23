package ipqualityrunner

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/modules/ipqualityrunner/script"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

// runTimeout bounds one run of the pinned script.
const runTimeout = 15 * time.Minute

// HostCommands and HostPackages are what the installer provides for the
// pinned script.
var (
	HostCommands = strings.Fields(script.RunnerCommands)
	HostPackages = strings.Fields(script.RunnerPackages)
)

type Handler struct {
	provider      *script.Provider
	scriptVersion string
}

// New runs the pinned script kept under the Agent root.
func New(root string) (*Handler, error) {
	provider, err := script.New(script.Config{
		ScriptPath: script.PinnedPath(root), Timeout: runTimeout,
		ScriptVersion: script.PinnedVersion, ExpectedSHA256Hex: script.PinnedSHA256,
	})
	if err != nil {
		return nil, err
	}
	return &Handler{provider: provider, scriptVersion: script.PinnedVersion}, nil
}

// PrepareScript downloads the pinned script under root unless it is there.
func PrepareScript(ctx context.Context, client *http.Client, root string) error {
	return script.EnsurePinnedScript(ctx, client, script.PinnedPath(root))
}

// RemoveStaleScripts deletes the scripts of earlier pins under root. Only a
// committed deployment calls it: a later candidate downloads its own pin.
func RemoveStaleScripts(root string) error {
	return script.RemoveOtherScripts(filepath.Dir(script.PinnedPath(root)))
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

func (h *Handler) Failed(_ protocol.OperationOffer, code string) protocol.ExecutionResult {
	return h.failure(code, "", "", "")
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
