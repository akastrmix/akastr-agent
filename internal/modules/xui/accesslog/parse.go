package accesslog

import (
	"bytes"
	"net/netip"
	"strings"
)

// parse reads one Xray access line that accepted a user's request:
//
//	2026/10/12 12:42:51.677607 from 1.2.3.4:33704 accepted tcp:example.com:443 [in >> out] email: user
//
// The source may carry a tcp: or udp: prefix and may be IPv6 in brackets.
func parse(raw []byte) (netip.AddrPort, string, bool) {
	_, rest, found := bytes.Cut(raw, []byte(" from "))
	if !found {
		return netip.AddrPort{}, "", false
	}
	source, rest, found := bytes.Cut(rest, []byte(" accepted "))
	if !found {
		return netip.AddrPort{}, "", false
	}
	at := bytes.LastIndex(rest, []byte(" email: "))
	if at < 0 {
		return netip.AddrPort{}, "", false
	}
	email := string(rest[at+len(" email: "):])
	if email == "" || strings.ContainsAny(email, " \t") {
		return netip.AddrPort{}, "", false
	}
	text := strings.TrimPrefix(strings.TrimPrefix(string(source), "tcp:"), "udp:")
	addr, err := netip.ParseAddrPort(text)
	if err != nil || addr.Port() == 0 {
		return netip.AddrPort{}, "", false
	}
	return netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port()), email, true
}
