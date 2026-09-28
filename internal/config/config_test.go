package config

import (
	"strings"
	"testing"
)

const validTarget = `{"schema_version":4,"configuration_revision":2,"mode":"target",
"agent_id":"123e4567-e89b-42d3-a456-426614174102","name":"HKT",
"control_endpoint":"wss://origin.example.com/internal/agents/ws",
"target":{"ip_watch_interval_seconds":60,"observe_ipv6":true,
"change_ip":{"provider":"command","program":"/usr/local/bin/changeip","args":["--now"]},
"socks5":{"enabled":true,"port":1080}}}`

func TestParseFailsClosed(t *testing.T) {
	if _, err := Parse([]byte(validTarget)); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	for name, input := range map[string]string{
		"unknown field":      strings.Replace(validTarget, `"name":"HKT"`, `"name":"HKT","extra":true`, 1),
		"shell entry point":  strings.Replace(validTarget, `/usr/local/bin/changeip`, `/bin/sh`, 1),
		"hidden program":     strings.Replace(validTarget, `/usr/local/bin/changeip`, `/root/changeip`, 1),
		"runner and target":  strings.Replace(validTarget, `"socks5"`, `"runner":{},"socks5"`, 1),
		"trailing document":  validTarget + `{}`,
		"future schema":      strings.Replace(validTarget, `"schema_version":4`, `"schema_version":5`, 1),
		"plain HTTP control": strings.Replace(validTarget, `wss://`, `ws://`, 1),
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
