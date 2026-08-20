package app

import (
	"testing"

	"github.com/akastrmix/akastr-agent/internal/config"
)

func TestBuildCapabilitiesOmitsSecretsAndLocalPaths(t *testing.T) {
	registry, err := buildCapabilities(config.CapabilitiesConfig{
		ChangeIP: config.ChangeIPConfig{Provider: "command", Program: "/bin/bash", Args: []string{"/root/changeip.sh"}},
		SOCKS5:   config.SOCKS5Config{Enabled: true, Port: 1080},
		IPQualityRunner: config.IPQualityRunnerConfig{
			Enabled: true, ScriptPath: "/opt/ipquality/ip.sh", ProxyProfilesFile: "/etc/proxies.json", MaxConcurrency: 1, ScriptVersion: "v2026.08.13",
		},
	}, []string{"backup", "primary"})
	if err != nil {
		t.Fatal(err)
	}
	listed := registry.List()
	if len(listed) != 3 {
		t.Fatalf("capabilities = %#v", listed)
	}
	for _, descriptor := range listed {
		for _, value := range descriptor.Properties {
			if value == "/bin/bash" || value == "/root/changeip.sh" || value == "/etc/proxies.json" {
				t.Fatalf("capability leaked local path: %#v", descriptor)
			}
		}
	}
	var socks5Properties map[string]any
	var runnerProperties map[string]any
	for _, descriptor := range listed {
		if descriptor.Name == "proxy.socks5" {
			socks5Properties = descriptor.Properties
		}
		if descriptor.Name == "ipquality.runner" {
			runnerProperties = descriptor.Properties
		}
	}
	if len(socks5Properties) != 1 || socks5Properties["port"] != "1080" {
		t.Fatalf("SOCKS5 capability properties = %#v, want port only", socks5Properties)
	}
	profiles, ok := runnerProperties["proxy_profile_ids"].([]string)
	if !ok || len(profiles) != 2 || profiles[0] != "backup" || profiles[1] != "primary" {
		t.Fatalf("runner proxy profiles = %#v", runnerProperties["proxy_profile_ids"])
	}
}
