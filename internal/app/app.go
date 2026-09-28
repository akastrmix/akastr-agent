package app

import (
	"fmt"
	"sort"
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
	return NewModel(cfg)
}

func NewModel(cfg config.Config) (*Model, error) {
	registry, err := buildCapabilities(cfg)
	if err != nil {
		return nil, err
	}
	return &Model{Config: cfg, Capabilities: registry}, nil
}

func buildCapabilities(cfg config.Config) (*capability.Registry, error) {
	descriptors := make([]capability.Descriptor, 0, 4)
	if target := cfg.Target; target != nil {
		descriptors = append(descriptors, capability.Descriptor{
			Name:    "ip.observe",
			Version: 1,
			Properties: map[string]any{
				"interval_seconds": strconv.Itoa(target.IPWatchIntervalSeconds),
				"observe_ipv6":     strconv.FormatBool(*target.ObserveIPv6),
			},
		})
		if target.ChangeIP.Provider != "disabled" {
			descriptors = append(descriptors, capability.Descriptor{
				Name:            "changeip.command",
				Version:         1,
				ExclusiveGroups: []string{"target-network"},
			})
		}
		if target.SOCKS5.Enabled {
			descriptors = append(descriptors, capability.Descriptor{
				Name:       "proxy.socks5",
				Version:    1,
				Properties: map[string]any{"port": strconv.Itoa(target.SOCKS5.Port)},
			})
		}
	}
	if runner := cfg.Runner; runner != nil {
		profileIDs := make([]string, 0, len(runner.Profiles))
		for _, profile := range runner.Profiles {
			profileIDs = append(profileIDs, profile.ID)
		}
		sort.Strings(profileIDs)
		descriptors = append(descriptors, capability.Descriptor{
			Name:            "ipquality.runner",
			Version:         1,
			ExclusiveGroups: []string{"ipquality-runner"},
			Properties: map[string]any{
				"max_concurrency":   "1",
				"script_version":    qualityscript.PinnedVersion,
				"proxy_profile_ids": profileIDs,
			},
		})
	}
	registry, err := capability.New(descriptors...)
	if err != nil {
		return nil, fmt.Errorf("build capability registry: %w", err)
	}
	return registry, nil
}
