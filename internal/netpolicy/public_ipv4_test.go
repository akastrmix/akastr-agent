package netpolicy

import (
	"net/netip"
	"testing"
)

func TestIsPublicIPv4(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"1.1.1.1": true, "8.8.8.8": true,
		"0.1.2.3": false, "10.0.0.1": false, "100.64.0.1": false,
		"127.0.0.1": false, "169.254.1.1": false, "172.16.0.1": false,
		"192.0.0.1": false, "192.0.2.1": false, "192.88.99.1": false,
		"192.168.1.1": false, "198.18.0.1": false, "198.51.100.1": false,
		"203.0.113.1": false, "224.0.0.1": false, "240.0.0.1": false,
		"::1": false,
	}
	for input, expected := range tests {
		input, expected := input, expected
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			if actual := IsPublicIPv4(netip.MustParseAddr(input)); actual != expected {
				t.Fatalf("IsPublicIPv4(%s) = %v, want %v", input, actual, expected)
			}
		})
	}
}
