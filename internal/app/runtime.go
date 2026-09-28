package app

import (
	"context"
	"errors"
	"time"

	changefeature "github.com/akastrmix/akastr-agent/internal/features/changeip"
	"github.com/akastrmix/akastr-agent/internal/features/ipqualityrunner"
	"github.com/akastrmix/akastr-agent/internal/features/ipwatch"
	"github.com/akastrmix/akastr-agent/internal/layout"
	"github.com/akastrmix/akastr-agent/internal/operation"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	changeprovider "github.com/akastrmix/akastr-agent/internal/providers/changeip"
	changecommand "github.com/akastrmix/akastr-agent/internal/providers/changeip/command"
	changehttp "github.com/akastrmix/akastr-agent/internal/providers/changeip/httpcurl"
	qualityscript "github.com/akastrmix/akastr-agent/internal/providers/ipquality/script"
)

// Fixed runtime limits. Cloud configures what a node does, not these bounds.
const (
	recentOperationLimit  = 64
	changeIPTimeout       = time.Minute
	changeIPObserveWindow = 5 * time.Minute
	ipQualityTimeout      = 15 * time.Minute
	observationTimeout    = 10 * time.Second
)

type Runtime struct {
	operations *operation.Executor
	changeIP   *changefeature.Handler
	ipQuality  *ipqualityrunner.Handler
	ipMonitor  *ipwatch.Monitor
}

// BuildRuntime validates every local dependency the configuration needs. It
// only reads durable state, so update candidates may call it before activation.
func BuildRuntime(model *Model, paths layout.Layout) (*Runtime, error) {
	engine, err := operation.Open(operation.Options{
		StateFile: paths.StateFile(), RecentLimit: recentOperationLimit,
	})
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{operations: operation.NewExecutor(engine)}
	if target := model.Config.Target; target != nil {
		observer, err := ipwatch.New(observationTimeout, "Akastr-Agent")
		if err != nil {
			return nil, err
		}
		runtime.ipMonitor, err = ipwatch.OpenMonitor(
			paths.IPStateFile(), observer,
			time.Duration(target.IPWatchIntervalSeconds)*time.Second, *target.ObserveIPv6,
		)
		if err != nil {
			return nil, err
		}
		var provider changeprovider.Provider
		switch target.ChangeIP.Provider {
		case "disabled":
		case "http_bearer":
			provider, err = changehttp.New(changehttp.Config{
				Program: "/usr/bin/curl", URL: target.ChangeIP.URL,
				BearerToken: target.ChangeIP.BearerToken, Timeout: changeIPTimeout,
			})
		case "command":
			provider, err = changecommand.New(changecommand.Config{
				Program: target.ChangeIP.Program, Args: target.ChangeIP.Args, Timeout: changeIPTimeout,
			})
		default:
			err = errors.New("ChangeIP provider type is unsupported")
		}
		if err != nil {
			return nil, err
		}
		if provider != nil {
			runtime.changeIP = changefeature.New(observer, provider, runtime.ipMonitor, changeIPObserveWindow)
		}
	}
	if runner := model.Config.Runner; runner != nil {
		profiles := make(map[string]qualityscript.Profile, len(runner.Profiles))
		for _, profile := range runner.Profiles {
			profiles[profile.ID] = qualityscript.Profile{Username: profile.Username, Password: profile.Password}
		}
		provider, err := qualityscript.New(qualityscript.Config{
			ScriptPath: paths.IPQualityScript(qualityscript.PinnedSHA256), Profiles: profiles,
			Timeout: ipQualityTimeout, ScriptVersion: qualityscript.PinnedVersion,
			ExpectedSHA256Hex: qualityscript.PinnedSHA256,
		})
		if err != nil {
			return nil, err
		}
		runtime.ipQuality = ipqualityrunner.New(provider, qualityscript.PinnedVersion)
	}
	return runtime, nil
}

func (r *Runtime) IPMonitor() *ipwatch.Monitor {
	return r.ipMonitor
}

func (r *Runtime) Execute(ctx context.Context, offer protocol.OperationOffer) (protocol.ExecutionResult, error) {
	switch offer.CommandType {
	case "changeip.execute":
		if r.changeIP == nil {
			return protocol.ExecutionResult{}, errors.New("accepted ChangeIP command has no local capability")
		}
		return r.operations.Execute(ctx, offer, "target-network", r.changeIP)
	case "ipquality.execute":
		if r.ipQuality == nil {
			return protocol.ExecutionResult{}, errors.New("accepted IPQuality command has no local capability")
		}
		return r.operations.Execute(ctx, offer, "ipquality-runner", r.ipQuality)
	default:
		return protocol.ExecutionResult{}, errors.New("accepted command type is unsupported")
	}
}
