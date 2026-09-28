package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/capability"
	changefeature "github.com/akastrmix/akastr-agent/internal/features/changeip"
	"github.com/akastrmix/akastr-agent/internal/features/ipqualityrunner"
	"github.com/akastrmix/akastr-agent/internal/features/ipwatch"
	"github.com/akastrmix/akastr-agent/internal/features/socks5"
	"github.com/akastrmix/akastr-agent/internal/layout"
	changeprovider "github.com/akastrmix/akastr-agent/internal/providers/changeip"
	changecommand "github.com/akastrmix/akastr-agent/internal/providers/changeip/command"
	changehttp "github.com/akastrmix/akastr-agent/internal/providers/changeip/httpcurl"
	qualityscript "github.com/akastrmix/akastr-agent/internal/providers/ipquality/script"
)

// Fixed runtime limits. Cloud configures what a node does, not these bounds.
const (
	changeIPTimeout       = time.Minute
	changeIPObserveWindow = 5 * time.Minute
	ipQualityTimeout      = 15 * time.Minute
	observationTimeout    = 10 * time.Second
)

// modules holds the parsed configuration of every module a node can enable.
// Adding a capability means one package under internal/features plus one
// field and its cases in this file.
type modules struct {
	ipWatch  *ipwatch.Config
	changeIP *changefeature.Config
	socks5   *socks5.Config
	runner   *ipqualityrunner.Config
}

func parseModules(sections map[string]json.RawMessage) (modules, error) {
	var m modules
	for name, raw := range sections {
		var err error
		switch name {
		case ipwatch.Name:
			m.ipWatch, err = parseSection(ipwatch.ParseConfig, raw)
		case changefeature.Name:
			m.changeIP, err = parseSection(changefeature.ParseConfig, raw)
		case socks5.Name:
			m.socks5, err = parseSection(socks5.ParseConfig, raw)
		case ipqualityrunner.Name:
			m.runner, err = parseSection(ipqualityrunner.ParseConfig, raw)
		default:
			return modules{}, fmt.Errorf("configuration module %q is unknown to this Agent", name)
		}
		if err != nil {
			return modules{}, fmt.Errorf("module %s: %w", name, err)
		}
	}
	// ChangeIP is confirmed only through the observed public IPv4.
	if m.changeIP != nil && m.ipWatch == nil {
		return modules{}, errors.New("module changeip requires ip_watch")
	}
	return m, nil
}

func parseSection[T any](parse func(json.RawMessage) (T, error), raw json.RawMessage) (*T, error) {
	cfg, err := parse(raw)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (m modules) capabilities() []capability.Descriptor {
	var descriptors []capability.Descriptor
	if m.ipWatch != nil {
		descriptors = append(descriptors, m.ipWatch.Capability())
	}
	if m.changeIP != nil {
		descriptors = append(descriptors, m.changeIP.Capability())
	}
	if m.socks5 != nil {
		descriptors = append(descriptors, m.socks5.Capability())
	}
	if m.runner != nil {
		descriptors = append(descriptors, m.runner.Capability())
	}
	return descriptors
}

// hostCommands and hostPackages are what the installer provides with apt.
func (m modules) hostCommands() ([]string, []string) {
	if m.runner == nil {
		return nil, nil
	}
	return strings.Fields(qualityscript.RunnerCommands), strings.Fields(qualityscript.RunnerPackages)
}

// prepare fetches pinned module assets before the runtime is built.
func (m modules) prepare(ctx context.Context, paths layout.Layout, client *http.Client) error {
	if m.runner != nil {
		return qualityscript.EnsurePinnedScript(ctx, client, paths.IPQualityScript(qualityscript.PinnedSHA256))
	}
	return nil
}

func (m modules) build(paths layout.Layout, runtime *Runtime) error {
	if m.ipWatch != nil {
		observer, err := ipwatch.New(observationTimeout, "Akastr-Agent")
		if err != nil {
			return err
		}
		monitor, err := ipwatch.OpenMonitor(paths.IPStateFile(), observer,
			time.Duration(m.ipWatch.IntervalSeconds)*time.Second, m.ipWatch.IPv6)
		if err != nil {
			return err
		}
		runtime.reporters = append(runtime.reporters, ipwatch.Reporter{Monitor: monitor})
		if m.changeIP != nil {
			var provider changeprovider.Provider
			if m.changeIP.Provider == "http_bearer" {
				provider, err = changehttp.New(changehttp.Config{
					Program: "/usr/bin/curl", URL: m.changeIP.URL,
					BearerToken: m.changeIP.BearerToken, Timeout: changeIPTimeout,
				})
			} else {
				provider, err = changecommand.New(changecommand.Config{
					Program: m.changeIP.Program, Args: m.changeIP.Args, Timeout: changeIPTimeout,
				})
			}
			if err != nil {
				return err
			}
			runtime.addCommands(changefeature.New(observer, provider, monitor, changeIPObserveWindow))
		}
	}
	if m.runner != nil {
		provider, err := qualityscript.New(qualityscript.Config{
			ScriptPath: paths.IPQualityScript(qualityscript.PinnedSHA256), Profiles: m.runner.ScriptProfiles(),
			Timeout: ipQualityTimeout, ScriptVersion: qualityscript.PinnedVersion,
			ExpectedSHA256Hex: qualityscript.PinnedSHA256,
		})
		if err != nil {
			return err
		}
		runtime.addCommands(ipqualityrunner.New(provider, qualityscript.PinnedVersion))
	}
	return nil
}
