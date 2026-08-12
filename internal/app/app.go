package app

import (
	"fmt"
	"strconv"

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/config"
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
	registry, err := buildCapabilities(cfg.Capabilities)
	if err != nil {
		return nil, err
	}
	return &Model{Config: cfg, Capabilities: registry}, nil
}

func buildCapabilities(cfg config.CapabilitiesConfig) (*capability.Registry, error) {
	descriptors := make([]capability.Descriptor, 0, 4)
	if cfg.IPWatch.Enabled {
		descriptors = append(descriptors, capability.Descriptor{
			Name:    "ip.observe",
			Version: 1,
			Properties: map[string]string{
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
		descriptors = append(descriptors, capability.Descriptor{
			Name:    "proxy.socks5",
			Version: 1,
			Properties: map[string]string{
				"address_source": cfg.SOCKS5.AddressSource,
				"port":           strconv.Itoa(cfg.SOCKS5.Port),
			},
		})
	}
	if cfg.IPQualityRunner.Enabled {
		descriptors = append(descriptors, capability.Descriptor{
			Name:            "ipquality.runner",
			Version:         1,
			ExclusiveGroups: []string{"ipquality-runner"},
			Properties: map[string]string{
				"max_concurrency": strconv.Itoa(cfg.IPQualityRunner.MaxConcurrency),
			},
		})
	}
	registry, err := capability.New(descriptors...)
	if err != nil {
		return nil, fmt.Errorf("build capability registry: %w", err)
	}
	return registry, nil
}
