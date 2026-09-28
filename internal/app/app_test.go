package app

import (
	"testing"

	"github.com/akastrmix/akastr-agent/internal/config"
)

func TestCapabilitiesOmitSecretsAndLocalPaths(t *testing.T) {
	observeIPv6 := true
	target, err := buildCapabilities(config.Config{Target: &config.Target{
		IPWatchIntervalSeconds: 60, ObserveIPv6: &observeIPv6,
		ChangeIP: config.ChangeIP{Provider: "command", Program: "/usr/local/bin/changeip", Args: []string{"secret-arg"}},
		SOCKS5:   config.SOCKS5{Enabled: true, Port: 1080},
	}})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := buildCapabilities(config.Config{Runner: &config.Runner{Profiles: []config.ProxyProfile{
		{ID: "primary", Username: "user", Password: "secret"},
		{ID: "backup", Username: "user", Password: "secret"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	listed := append(target.List(), runner.List()...)
	if len(listed) != 4 {
		t.Fatalf("capabilities = %#v", listed)
	}
	for _, descriptor := range listed {
		for _, value := range descriptor.Properties {
			if value == "/usr/local/bin/changeip" || value == "secret-arg" || value == "secret" || value == "user" {
				t.Fatalf("capability leaked local configuration: %#v", descriptor)
			}
		}
		switch descriptor.Name {
		case "proxy.socks5":
			if len(descriptor.Properties) != 1 || descriptor.Properties["port"] != "1080" {
				t.Fatalf("SOCKS5 capability properties = %#v, want port only", descriptor.Properties)
			}
		case "ipquality.runner":
			profiles, ok := descriptor.Properties["proxy_profile_ids"].([]string)
			if !ok || len(profiles) != 2 || profiles[0] != "backup" || profiles[1] != "primary" {
				t.Fatalf("runner proxy profiles = %#v", descriptor.Properties["proxy_profile_ids"])
			}
		}
	}
}
