package app

import (
	"fmt"
	"strconv"

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/config"
	qualityscript "github.com/akastrmix/akastr-agent/internal/providers/ipquality/script"
)

type Model struct {
	Config       config.Config
	Capabilities *capability.Registry
}

func Load(configPath string) (*Model, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	var runnerProfileIDs []string
	if cfg.Capabilities.IPQualityRunner.Enabled {
		runnerProfileIDs, err = qualityscript.ProfileIDs(cfg.Capabilities.IPQualityRunner.ProxyProfilesFile)
		if err != nil {
			return nil, fmt.Errorf("load IPQuality profile identifiers: %w", err)
		}
	}
	registry, err := buildCapabilities(cfg.Capabilities, runnerProfileIDs)
	if err != nil {
		return nil, err
	}
	return &Model{Config: cfg, Capabilities: registry}, nil
}

func buildCapabilities(cfg config.CapabilitiesConfig, runnerProfileIDs []string) (*capability.Registry, error) {
	descriptors := make([]capability.Descriptor, 0, 4)
	if cfg.IPWatch.Enabled {
		descriptors = append(descriptors, capability.Descriptor{
			Name:    "ip.observe",
			Version: 1,
			Properties: map[string]any{
				"interval_seconds": strconv.Itoa(cfg.IPWatch.IntervalSeconds),
				"observe_ipv6":     strconv.FormatBool(cfg.IPWatch.ObserveIPv6),
			},
		})
	}
	if cfg.ChangeIP.Enabled {
		descriptors = append(descriptors, capability.Descriptor{
			Name:            "changeip.command",
			Version:         1,
			ExclusiveGroups: []string{"target-network"},
		})
	}
	if cfg.SOCKS5.Enabled {
		properties := map[string]any{
			"port": strconv.Itoa(cfg.SOCKS5.Port),
		}
		descriptors = append(descriptors, capability.Descriptor{
			Name:       "proxy.socks5",
			Version:    1,
			Properties: properties,
		})
	}
	if cfg.IPQualityRunner.Enabled {
		descriptors = append(descriptors, capability.Descriptor{
			Name:            "ipquality.runner",
			Version:         1,
			ExclusiveGroups: []string{"ipquality-runner"},
			Properties: map[string]any{
				"max_concurrency":   strconv.Itoa(cfg.IPQualityRunner.MaxConcurrency),
				"script_version":    cfg.IPQualityRunner.ScriptVersion,
				"proxy_profile_ids": append([]string(nil), runnerProfileIDs...),
			},
		})
	}
	registry, err := capability.New(descriptors...)
	if err != nil {
		return nil, fmt.Errorf("build capability registry: %w", err)
	}
	return registry, nil
}
