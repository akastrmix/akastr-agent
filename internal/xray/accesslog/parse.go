package accesslog

import (
	"bytes"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// entry is one access log line that accepted a user's request.
type entry struct {
	// at is the line's time, zero when it has none.
	at       time.Time
	tag      string
	addr     netip.AddrPort
	email    string
	dest     Dest
	outbound string
	// backlog is set for lines read on opening the log, which may be old.
	backlog bool
}

// stampLayout is how Xray starts each line, in the local time zone.
const stampLayout = "2006/01/02 15:04:05.999999"

// maxHost bounds a destination name; Xray's protocols carry at most 255 bytes.
const maxHost = 255

// parse reads one Xray access line that accepted a user's request:
//
//	2026/10/12 12:42:51.677607 from 1.2.3.4:33704 accepted tcp:example.com:443 [in >> out] email: user
//
// It returns the time, the inbound's tag, the source, the user, the
// destination and the outbound's tag. The source may carry a tcp: or udp:
// prefix and may be IPv6 in brackets. A destination it cannot read is left
// empty: the line still says whose the connection is.
func parse(raw []byte) (entry, bool) {
	stamp, rest, found := bytes.Cut(raw, []byte(" from "))
	if !found {
		return entry{}, false
	}
	source, rest, found := bytes.Cut(rest, []byte(" accepted "))
	if !found {
		return entry{}, false
	}
	at := bytes.LastIndex(rest, []byte(" email: "))
	if at < 0 {
		return entry{}, false
	}
	email := string(rest[at+len(" email: "):])
	if email == "" || strings.ContainsAny(email, " \t") {
		return entry{}, false
	}
	// The route is "[inbound >> outbound]": ">>" for the default outbound,
	// "->" for a routing rule's, "==>" for one the platform chose.
	route := rest[:at]
	open := bytes.LastIndexByte(route, '[')
	if open < 0 || !bytes.HasSuffix(route, []byte("]")) {
		return entry{}, false
	}
	detour := string(route[open+1 : len(route)-1])
	tag, _, found := strings.Cut(detour, " ")
	if !found || tag == "" {
		return entry{}, false
	}
	text := strings.TrimPrefix(strings.TrimPrefix(string(source), "tcp:"), "udp:")
	addr, err := netip.ParseAddrPort(text)
	if err != nil || addr.Port() == 0 {
		return entry{}, false
	}
	when, err := time.ParseInLocation(stampLayout, string(stamp), time.Local)
	if err != nil {
		when = time.Time{}
	}
	return entry{at: when, tag: tag, addr: netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port()), email: email,
		dest: parseDest(strings.TrimSuffix(string(route[:open]), " ")), outbound: detour[strings.LastIndexByte(detour, ' ')+1:]}, true
}

// parseDest reads a destination as Xray writes it: tcp:example.com:443,
// udp:8.8.8.8:53 or tcp:[2001:db8::1]:443. The client chooses the host, so
// only the characters of names and addresses are read: others grow when
// escaped and could make a report outgrow a protocol message.
func parseDest(s string) Dest {
	network, hostport, _ := strings.Cut(s, ":")
	host, port, err := net.SplitHostPort(hostport)
	n, portErr := strconv.ParseUint(port, 10, 16)
	if (network != "tcp" && network != "udp") || err != nil || portErr != nil || n == 0 || !plainHost(host) {
		return Dest{}
	}
	return Dest{Network: network, Host: host, Port: uint16(n)}
}

// plainHost also refuses a name that is only a final dot, which Cloud drops.
func plainHost(host string) bool {
	if strings.TrimSuffix(host, ".") == "" || len(host) > maxHost {
		return false
	}
	for _, c := range []byte(host) {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '.' || c == '-' || c == '_' || c == ':') {
			return false
		}
	}
	return true
}
