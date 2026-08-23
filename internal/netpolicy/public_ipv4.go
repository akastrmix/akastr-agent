package netpolicy

import "net/netip"

var nonPublicIPv4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

type ipv6SpecialPurpose struct {
	prefix            netip.Prefix
	globallyReachable bool
}

var specialPurposeIPv6 = []ipv6SpecialPurpose{
	// Globally reachable exceptions inside IANA's otherwise non-global 2001::/23 block.
	{netip.MustParsePrefix("2001:1::1/128"), true},
	{netip.MustParsePrefix("2001:1::2/128"), true},
	{netip.MustParsePrefix("2001:1::3/128"), true},
	{netip.MustParsePrefix("2001:3::/32"), true},
	{netip.MustParsePrefix("2001:4:112::/48"), true},
	{netip.MustParsePrefix("2001:20::/28"), true},
	{netip.MustParsePrefix("2001:30::/28"), true},
	{netip.MustParsePrefix("64:ff9b:1::/48"), false},
	{netip.MustParsePrefix("100::/64"), false},
	{netip.MustParsePrefix("100:0:0:1::/64"), false},
	{netip.MustParsePrefix("2001::/23"), false},
	{netip.MustParsePrefix("2001:db8::/32"), false},
	{netip.MustParsePrefix("2002::/16"), false},
	{netip.MustParsePrefix("3fff::/20"), false},
	{netip.MustParsePrefix("5f00::/16"), false},
}

func IsPublicIPv4(address netip.Addr) bool {
	address = address.Unmap()
	if !address.Is4() {
		return false
	}
	for _, prefix := range nonPublicIPv4 {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func IsPublicIPv6(address netip.Addr) bool {
	if !address.Is6() || address.Is4In6() || !address.IsGlobalUnicast() || address.IsPrivate() ||
		address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	for _, special := range specialPurposeIPv6 {
		if special.prefix.Contains(address) {
			return special.globallyReachable
		}
	}
	return true
}
