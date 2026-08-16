package app

import (
	"context"
	"time"

	changefeature "github.com/akastrmix/akastr-agent/internal/features/changeip"
	"github.com/akastrmix/akastr-agent/internal/features/ipqualityrunner"
	"github.com/akastrmix/akastr-agent/internal/features/ipwatch"
	"github.com/akastrmix/akastr-agent/internal/operation"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	changecommand "github.com/akastrmix/akastr-agent/internal/providers/changeip/command"
	qualityscript "github.com/akastrmix/akastr-agent/internal/providers/ipquality/script"
)

type Runtime struct {
	changeIP  *changefeature.Handler
	ipQuality *ipqualityrunner.Handler
	ipMonitor *ipwatch.Monitor
}

func BuildRuntime(model *Model) (*Runtime, error) {
	engine, err := operation.Open(operation.Options{
		StateFile: model.Config.StateFile, RecentLimit: model.Config.RecentOperationLimit,
	})
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{}
	var observer *ipwatch.Observer
	if model.Config.Capabilities.ChangeIP.Enabled || model.Config.Capabilities.IPWatch.Enabled {
		observer, err = ipwatch.New(10*time.Second, "Akastr-Agent")
		if err != nil {
			return nil, err
		}
	}
	if model.Config.Capabilities.IPWatch.Enabled {
		runtime.ipMonitor, err = ipwatch.OpenMonitor(
			model.Config.IPStateFile, observer,
			time.Duration(model.Config.Capabilities.IPWatch.IntervalSeconds)*time.Second,
		)
		if err != nil {
			return nil, err
		}
	}
	if model.Config.Capabilities.ChangeIP.Enabled {
		provider, err := changecommand.New(changecommand.Config{
			Program: model.Config.Capabilities.ChangeIP.Program,
			Args:    model.Config.Capabilities.ChangeIP.Args,
			Timeout: time.Duration(model.Config.Capabilities.ChangeIP.TimeoutSeconds) * time.Second,
		})
		if err != nil {
			return nil, err
		}
		runtime.changeIP = changefeature.New(
			engine, observer, provider,
			time.Duration(model.Config.Capabilities.ChangeIP.ObserveTimeoutSeconds)*time.Second,
		)
	}
	if model.Config.Capabilities.IPQualityRunner.Enabled {
		cfg := model.Config.Capabilities.IPQualityRunner
		provider, err := qualityscript.New(qualityscript.Config{
			ScriptPath: cfg.ScriptPath, ProfilesFile: cfg.ProxyProfilesFile,
			Timeout:       time.Duration(cfg.TimeoutSeconds) * time.Second,
			ScriptVersion: cfg.ScriptVersion, ExpectedSHA256Hex: cfg.ScriptSHA256,
		})
		if err != nil {
			return nil, err
		}
		runtime.ipQuality = ipqualityrunner.New(engine, provider, cfg.ScriptVersion)
	}
	return runtime, nil
}

func (r *Runtime) IPMonitor() *ipwatch.Monitor {
	return r.ipMonitor
}

func (r *Runtime) Execute(ctx context.Context, offer protocol.OperationOffer) protocol.ExecutionResult {
	now := time.Now()
	if now.Before(offer.NotBefore) {
		return unsupportedResult(offer.CommandType, "offer_not_ready")
	}
	if !now.Before(offer.ExpiresAt) {
		return unsupportedResult(offer.CommandType, "offer_expired")
	}
	switch offer.CommandType {
	case "changeip.execute":
		if r.changeIP == nil {
			return unsupportedResult(offer.CommandType, "capability_disabled")
		}
		return r.changeIP.Execute(ctx, offer)
	case "ipquality.execute":
		if r.ipQuality == nil {
			return unsupportedResult(offer.CommandType, "capability_disabled")
		}
		return r.ipQuality.Execute(ctx, offer)
	default:
		return protocol.ExecutionResult{Outcome: "failed", Code: "command_unsupported", Result: map[string]any{}}
	}
}

func unsupportedResult(commandType, code string) protocol.ExecutionResult {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if commandType == "changeip.execute" {
		return protocol.ExecutionResult{Outcome: "failed", Code: code, Result: map[string]any{
			"old_ipv4": nil, "new_ipv4": nil, "observed_at": now,
		}}
	}
	if commandType == "ipquality.execute" {
		return protocol.ExecutionResult{Outcome: "failed", Code: code, Result: map[string]any{
			"report_url": nil, "proxy_ipv4_before": nil, "proxy_ipv4_after": nil,
			"script_version": "unavailable", "checked_at": now,
		}}
	}
	return protocol.ExecutionResult{Outcome: "failed", Code: code, Result: map[string]any{}}
}
