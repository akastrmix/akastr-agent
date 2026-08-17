package app

import (
	"context"
	"time"

	changefeature "github.com/akastrmix/akastr-agent/internal/features/changeip"
	"github.com/akastrmix/akastr-agent/internal/features/ipqualityrunner"
	"github.com/akastrmix/akastr-agent/internal/features/ipwatch"
	"github.com/akastrmix/akastr-agent/internal/operation"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	changeprovider "github.com/akastrmix/akastr-agent/internal/providers/changeip"
	changecommand "github.com/akastrmix/akastr-agent/internal/providers/changeip/command"
	changehttp "github.com/akastrmix/akastr-agent/internal/providers/changeip/httpcurl"
	qualityscript "github.com/akastrmix/akastr-agent/internal/providers/ipquality/script"
)

type Runtime struct {
	operations *operation.Engine
	changeIP   *changefeature.Handler
	ipQuality  *ipqualityrunner.Handler
	ipMonitor  *ipwatch.Monitor
}

func BuildRuntime(model *Model) (*Runtime, error) {
	engine, err := operation.Open(operation.Options{
		StateFile: model.Config.StateFile, RecentLimit: model.Config.RecentOperationLimit,
	})
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{operations: engine}
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
		cfg := model.Config.Capabilities.ChangeIP
		var provider changeprovider.Provider
		if cfg.Program == "/usr/bin/curl" && len(cfg.Args) == 2 && cfg.Args[0] == "--config" && cfg.Args[1] == "/etc/akastr-agent/changeip-curl.conf" {
			provider, err = changehttp.New(changehttp.Config{
				Program: cfg.Program, ConfigFile: cfg.Args[1],
				Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
			})
		} else {
			provider, err = changecommand.New(changecommand.Config{
				Program: cfg.Program, Args: cfg.Args,
				Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
			})
		}
		if err != nil {
			return nil, err
		}
		runtime.changeIP = changefeature.New(
			engine, observer, provider, runtime.ipMonitor,
			time.Duration(cfg.ObserveTimeoutSeconds)*time.Second,
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

func (r *Runtime) KnownOperation(commandID, commandType string) bool {
	if r.operations == nil {
		return false
	}
	if record, found := r.operations.Active(commandID); found {
		return record.Kind == commandType
	}
	if record, found := r.operations.Recent(commandID); found {
		return record.Kind == commandType
	}
	return false
}

func (r *Runtime) IPMonitor() *ipwatch.Monitor {
	return r.ipMonitor
}

func (r *Runtime) Execute(ctx context.Context, offer protocol.OperationOffer) protocol.ExecutionResult {
	now := time.Now()
	if now.Before(offer.NotBefore) {
		return unsupportedResult(offer.CommandType, "offer_not_ready")
	}
	if !now.Before(offer.ExpiresAt) && !r.KnownOperation(offer.CommandID, offer.CommandType) {
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
