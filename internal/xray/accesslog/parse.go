package accesslog

import (
	"bytes"
	"net/netip"
	"strings"
	"time"
)

// line is one access log line that accepted a user's request.
type line struct {
	// at is the line's time, zero when it has none.
	at    time.Time
	tag   string
	addr  netip.AddrPort
	email string
	// backlog is set for lines read on opening the log, which may be old.
	backlog bool
}

// stampLayout is how Xray starts each line, in the local time zone.
const stampLayout = "2006/01/02 15:04:05.999999"

// parse reads one Xray access line that accepted a user's request:
//
//	2026/10/12 12:42:51.677607 from 1.2.3.4:33704 accepted tcp:example.com:443 [in >> out] email: user
//
// It returns the time, the inbound's tag, the source and the user. The
// source may carry a tcp: or udp: prefix and may be IPv6 in brackets.
func parse(raw []byte) (line, bool) {
	stamp, rest, found := bytes.Cut(raw, []byte(" from "))
	if !found {
		return line{}, false
	}
	source, rest, found := bytes.Cut(rest, []byte(" accepted "))
	if !found {
		return line{}, false
	}
	at := bytes.LastIndex(rest, []byte(" email: "))
	if at < 0 {
		return line{}, false
	}
	email := string(rest[at+len(" email: "):])
	if email == "" || strings.ContainsAny(email, " \t") {
		return line{}, false
	}
	// The route is "[inbound >> outbound]", or "->" when it was not sniffed.
	route := rest[:at]
	open := bytes.LastIndexByte(route, '[')
	if open < 0 || !bytes.HasSuffix(route, []byte("]")) {
		return line{}, false
	}
	tag, _, found := strings.Cut(string(route[open+1:len(route)-1]), " ")
	if !found || tag == "" {
		return line{}, false
	}
	text := strings.TrimPrefix(strings.TrimPrefix(string(source), "tcp:"), "udp:")
	addr, err := netip.ParseAddrPort(text)
	if err != nil || addr.Port() == 0 {
		return line{}, false
	}
	when, err := time.ParseInLocation(stampLayout, string(stamp), time.Local)
	if err != nil {
		when = time.Time{}
	}
	return line{at: when, tag: tag, addr: netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port()), email: email}, true
}
