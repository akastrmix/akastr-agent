package firewall

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Table is the one nftables table the module owns, for IPv4 and IPv6 alike.
const Table = "akastr_firewall"

// The hit sets record what the ceilings and the final rule caught, bounded and
// expiring; a full set records no more but the rule that drops is a separate
// rule, so a full set never lets a packet through.
const (
	hitTimeout    = "1h"
	hitFinal      = "h_final"
	hitProtocol   = "h_proto"
	hitICMP4      = "h_icmp4"
	hitICMP6      = "h_icmp6"
	hitCeiling4   = "h_ceiling_4"
	hitCeiling6   = "h_ceiling_6"
	portHitSize   = 512
	otherHitSize  = 64
	markerPrefix  = "akastr:"
	digestLength  = 32
	tcpConnecting = "tcp flags & (fin|syn|rst|ack) == syn"
)

// Forwarding (Cloud ADR 0110). The forwards are members of a map and their
// ports members of one set per protocol (nft 1.0.6 cannot concatenate the
// original port); the default-route interface pairs of the transit check are
// members too. None of them is in the digest, so adding, changing or removing
// a forward, or a route moving, never replaces the table.
const (
	forwardMap  = "forwards"
	forwardTCP  = "forward_ports_tcp"
	forwardUDP  = "forward_ports_udp"
	transitSet4 = "transit_4"
	transitSet6 = "transit_6"
	// markerChain carries the marker on its one rule; every table has it.
	markerChain = "nat_prerouting"
)

// localGuard translates only packets addressed to this node, as Docker does
// with addrtype LOCAL: what a container or VM sends through the node to
// another host on a forwarded port is not redirected. Kernel tests replace it
// where nft_fib is not loaded, since they never load kernel modules.
var localGuard = "fib daddr type local"

var hitSizes = map[string]int{
	hitFinal: portHitSize, hitCeiling4: portHitSize, hitCeiling6: portHitSize,
	hitProtocol: otherHitSize, hitICMP4: otherHitSize, hitICMP6: otherHitSize,
}

// Control messages a host must accept (RFC 4890 4.4.1 for IPv6; IPv4 errors
// are also related to tracked connections). Ping is not here: it is a rule.
const guarantee = `ct state established,related accept
icmpv6 type { destination-unreachable, packet-too-big, time-exceeded, parameter-problem, mld-listener-query, mld-listener-report, mld-listener-done, nd-router-solicit, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert, ind-neighbor-solicit, ind-neighbor-advert, mld2-listener-report, 148, 149, 151, 152, 153 } accept
icmp type { destination-unreachable, time-exceeded, parameter-problem } accept
ip6 saddr fe80::/10 udp sport 547 udp dport 546 accept`

// Without the final drop this records what the default would catch. A TCP or
// UDP port counts only when a local socket would take the packet: scanners
// try every closed port and would otherwise fill the set within the hour,
// while what the owner needs to see is a service the rules leave out.
const finalRecord = tcpConnecting + ` socket transparent { 0, 1 } update @h_final { meta l4proto . th dport counter }
meta l4proto udp socket transparent { 0, 1 } update @h_final { meta l4proto . th dport counter }
meta l4proto icmp update @h_icmp4 { icmp type counter }
meta l4proto ipv6-icmp update @h_icmp6 { icmpv6 type counter }
meta l4proto != { tcp, udp, icmp, ipv6-icmp } update @h_proto { meta l4proto counter }`

// rendered is one target as nftables sees it: the rules, whose digest decides
// whether the installed table must be replaced, and the members, which change
// without replacing it: the target's address sets, and the forwarding members
// that follow every target on their own.
type rendered struct {
	mode       string
	digest     string
	rules      string
	members    map[string][]string
	forwarding map[string][]string
}

// placement is what the module puts in the table besides the target's sets:
// the forwards it applied, which can lag the target's when forwarding cannot
// be turned on, and the default-route interfaces of each family.
type placement struct {
	forwards           []Forward
	transit4, transit6 []string
}

// render turns a target into rules. resolved holds each domain's last
// addresses; a domain without any adds nobody.
func render(t Target, resolved map[string][]netip.Addr, p placement) rendered {
	r := rendered{mode: t.Mode, members: map[string][]string{}, forwarding: forwardingMembers(p)}
	if t.Mode != modeOff {
		r.members = members(t, resolved)
	}
	unmarked := structure(t, "")
	sum := sha256.Sum256([]byte(unmarked))
	r.digest = hex.EncodeToString(sum[:])[:digestLength]
	r.rules = structure(t, marker(t.Mode, r.digest))
	return r
}

func marker(mode, digest string) string { return markerPrefix + mode + ":" + digest }

// parseMarker reads the comment of the table's first rule.
func parseMarker(comment string) (mode, digest string, ok bool) {
	rest, found := strings.CutPrefix(comment, markerPrefix)
	if !found {
		return "", "", false
	}
	mode, digest, found = strings.Cut(rest, ":")
	if !found || (mode != modeOff && mode != modeObserve && mode != modeEnforce) || len(digest) != digestLength {
		return "", "", false
	}
	return mode, digest, true
}

// failureKey names a target for the record of one that failed confirmation;
// a retry from Cloud changes it without changing the rules.
func (r rendered) failureKey(retry int64) string {
	sum := sha256.Sum256([]byte(r.rules + "\nretry " + strconv.FormatInt(retry, 10)))
	return hex.EncodeToString(sum[:])[:digestLength]
}

// install replaces the whole table in one transaction.
func (r rendered) install() string {
	return remove() + r.rules + memberScript(r.members, false) + memberScript(r.forwarding, false)
}

// syncMembers replaces every address set's members in one transaction.
func (r rendered) syncMembers() string { return memberScript(r.members, true) }

// syncForwarding replaces the forwards, their ports and the transit
// interfaces in one transaction.
func (r rendered) syncForwarding() string { return memberScript(r.forwarding, true) }

func memberScript(members map[string][]string, flush bool) string {
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(members)) {
		if flush {
			kind := "set"
			if name == forwardMap {
				kind = "map"
			}
			fmt.Fprintf(&b, "flush %s inet %s %s\n", kind, Table, name)
		}
		if elements := members[name]; len(elements) > 0 {
			fmt.Fprintf(&b, "add element inet %s %s { %s }\n", Table, name, strings.Join(elements, ", "))
		}
	}
	return b.String()
}

// forwardingMembers lists the forwards as map entries and port set members,
// and each family's default-route interfaces as "in . out" pairs.
func forwardingMembers(p placement) map[string][]string {
	result := map[string][]string{forwardMap: {}, forwardTCP: {}, forwardUDP: {}, transitSet4: {}, transitSet6: {}}
	forwards := slices.Clone(p.forwards)
	slices.SortFunc(forwards, func(a, b Forward) int {
		return cmp.Or(cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(a.Port, b.Port))
	})
	for _, f := range forwards {
		result[forwardMap] = append(result[forwardMap], fmt.Sprintf("%s . %d : %s . %d", f.Protocol, f.Port, f.To, f.ToPort))
		ports := forwardTCP
		if f.Protocol == "udp" {
			ports = forwardUDP
		}
		result[ports] = append(result[ports], strconv.Itoa(f.Port))
	}
	for set, names := range map[string][]string{transitSet4: p.transit4, transitSet6: p.transit6} {
		for _, name := range slices.Sorted(slices.Values(names)) {
			result[set] = append(result[set], fmt.Sprintf("%q . %q", name, name))
		}
	}
	return result
}

// remove deletes the table whether or not it exists.
func remove() string {
	return fmt.Sprintf("table inet %s {}\ndelete table inet %s\n", Table, Table)
}

func memberSet(name string, family int) string { return fmt.Sprintf("m_%s_%d", name, family) }

func structure(t Target, mark string) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, "    "+format+"\n", args...) }
	fmt.Fprintf(&b, "table inet %s {\n", Table)
	// Mode off keeps only the forwarding: no sets, no ban, no input chain,
	// nothing recorded (Cloud ADR 0110).
	filtering := t.Mode != modeOff
	if filtering {
		for _, name := range slices.Sorted(maps.Keys(t.Sets)) {
			fmt.Fprintf(&b, "  set %s { type ipv4_addr; flags interval; }\n", memberSet(name, 4))
			fmt.Fprintf(&b, "  set %s { type ipv6_addr; flags interval; }\n", memberSet(name, 6))
		}
		hitTypes := map[string]string{
			hitFinal: "inet_proto . inet_service", hitProtocol: "inet_proto",
			hitICMP4: "icmp_type", hitICMP6: "icmpv6_type",
			hitCeiling4: "ipv4_addr . inet_proto . inet_service", hitCeiling6: "ipv6_addr . inet_proto . inet_service",
		}
		for _, name := range slices.Sorted(maps.Keys(hitTypes)) {
			fmt.Fprintf(&b, "  set %s { type %s; size %d; flags dynamic,timeout; timeout %s; }\n",
				name, hitTypes[name], hitSizes[name], hitTimeout)
		}
	}
	fmt.Fprintf(&b, "  map %s { type inet_proto . inet_service : ipv4_addr . inet_service; }\n", forwardMap)
	fmt.Fprintf(&b, "  set %s { type inet_service; }\n  set %s { type inet_service; }\n", forwardTCP, forwardUDP)
	fmt.Fprintf(&b, "  set %s { type ifname . ifname; }\n  set %s { type ifname . ifname; }\n", transitSet4, transitSet6)
	if filtering && t.Blocked != nil {
		// After connection tracking (-200) and before translation (-100), so
		// the ban covers forwarded traffic as well as this node's services.
		// Loopback first, as in input: traffic to the node's own address
		// crosses prerouting on lo.
		b.WriteString("  chain prerouting {\n")
		line("type filter hook prerouting priority mangle; policy accept;")
		line(`iif "lo" accept`)
		for _, f := range families {
			exempt := ""
			if t.Exempt != nil {
				exempt = fmt.Sprintf(" %s saddr != @%s", f.ip, memberSet(*t.Exempt, f.number))
			}
			line("ct direction original %s saddr @%s%s drop", f.ip, memberSet(*t.Blocked, f.number), exempt)
		}
		b.WriteString("  }\n")
	}
	fmt.Fprintf(&b, "  chain %s {\n", markerChain)
	line("type nat hook prerouting priority dstnat; policy accept;")
	translate := fmt.Sprintf("meta nfproto ipv4 %s dnat ip to meta l4proto . th dport map @%s", localGuard, forwardMap)
	if mark == "" {
		line("%s", translate)
	} else {
		line("%s comment %q", translate, mark)
	}
	b.WriteString("  }\n")
	// Masquerade only connections this module translated: the original
	// protocol and port are one of its forwards. Other programs' translations
	// keep their source.
	b.WriteString("  chain nat_postrouting {\n")
	line("type nat hook postrouting priority srcnat; policy accept;")
	for _, p := range [][2]string{{"tcp", forwardTCP}, {"udp", forwardUDP}} {
		line("meta nfproto ipv4 ct status dnat meta l4proto %s ct original proto-dst @%s masquerade", p[0], p[1])
	}
	b.WriteString("  }\n")
	// A packet that enters and leaves through the same default-route
	// interface untranslated is someone using this node as a router.
	// Containers, tunnels and bridges use other interfaces.
	b.WriteString("  chain filter_forward {\n")
	line("type filter hook forward priority filter; policy accept;")
	line("meta nfproto ipv4 iifname . oifname @%s ct status & (snat | dnat) == 0 drop", transitSet4)
	line("meta nfproto ipv6 iifname . oifname @%s ct status & (snat | dnat) == 0 drop", transitSet6)
	b.WriteString("  }\n")
	if !filtering {
		b.WriteString("}\n")
		return b.String()
	}
	b.WriteString("  chain input {\n")
	line("type filter hook input priority filter; policy accept;")
	line(`iif "lo" accept`)
	for _, rule := range strings.Split(guarantee, "\n") {
		line("%s", rule)
	}
	for _, c := range t.Ceilings {
		for _, f := range families {
			match := fmt.Sprintf("%s dport %s", c.Protocol, ports(c.Ports))
			for _, source := range c.Sources {
				match += fmt.Sprintf(" %s saddr != @%s", f.ip, memberSet(source, f.number))
			}
			connecting := ""
			if c.Protocol == "tcp" {
				connecting = " " + tcpConnecting
			}
			line("%s%s update @%s { %s saddr . meta l4proto . th dport counter }", match, connecting, f.ceilingHits, f.ip)
			if t.Mode == modeEnforce {
				line("%s drop", match)
			}
		}
	}
	for _, r := range t.Rules {
		verdict := "accept"
		if r.Action == "drop" {
			verdict = "drop"
		}
		for _, match := range ruleMatches(r) {
			line("%s %s", match, verdict)
		}
	}
	for _, rule := range strings.Split(finalRecord, "\n") {
		line("%s", rule)
	}
	if t.Mode == modeEnforce {
		line("drop")
	}
	b.WriteString("  }\n}\n")
	return b.String()
}

type family struct {
	number      int
	ip, icmp    string
	ceilingHits string
}

var families = []family{
	{number: 4, ip: "ip", icmp: "icmp", ceilingHits: hitCeiling4},
	{number: 6, ip: "ip6", icmp: "icmpv6", ceilingHits: hitCeiling6},
}

// ruleMatches expands one rule into nftables rules with the same verdict, one
// per source set and address family where it names sets.
func ruleMatches(r Rule) []string {
	protocol := func(f family) string {
		switch r.Protocol {
		case "tcp", "udp":
			return fmt.Sprintf("%s dport %s", r.Protocol, ports(r.Ports))
		case "ping":
			return f.icmp + " type echo-request"
		}
		return ""
	}
	join := func(parts ...string) string {
		return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), " ")
	}
	var matches []string
	if r.Sources.Any {
		if r.Protocol == "ping" {
			for _, f := range families {
				matches = append(matches, protocol(f))
			}
			return matches
		}
		// An empty match is a rule for every packet.
		return []string{protocol(families[0])}
	}
	for _, source := range r.Sources.Sets {
		for _, f := range families {
			matches = append(matches, join(fmt.Sprintf("%s saddr @%s", f.ip, memberSet(source, f.number)), protocol(f)))
		}
	}
	return matches
}

func ports(ranges [][2]int) string {
	parts := make([]string, 0, len(ranges))
	for _, p := range ranges {
		if p[0] == p[1] {
			parts = append(parts, strconv.Itoa(p[0]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", p[0], p[1]))
		}
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// members lists each set's addresses and resolved domains per family, without
// networks another member already covers: an interval set refuses overlaps.
func members(t Target, resolved map[string][]netip.Addr) map[string][]string {
	result := map[string][]string{}
	for name, set := range t.Sets {
		var prefixes []netip.Prefix
		for _, address := range set.Addresses {
			if prefix, err := parsePrefix(address); err == nil {
				prefixes = append(prefixes, prefix)
			}
		}
		for _, domain := range set.Domains {
			for _, address := range resolved[domain] {
				prefixes = append(prefixes, netip.PrefixFrom(address, address.BitLen()))
			}
		}
		for _, f := range families {
			result[memberSet(name, f.number)] = elements(prefixes, f.number == 4)
		}
	}
	return result
}

func elements(prefixes []netip.Prefix, ipv4 bool) []string {
	own := slices.DeleteFunc(slices.Clone(prefixes), func(p netip.Prefix) bool { return p.Addr().Is4() != ipv4 })
	slices.SortFunc(own, func(a, b netip.Prefix) int {
		return cmp.Or(a.Addr().Compare(b.Addr()), cmp.Compare(a.Bits(), b.Bits()))
	})
	var kept []netip.Prefix
	for _, p := range own {
		// Sorted by start and then by size, a covering network is the last kept.
		if len(kept) > 0 && kept[len(kept)-1].Bits() <= p.Bits() && kept[len(kept)-1].Contains(p.Addr()) {
			continue
		}
		kept = append(kept, p)
	}
	out := make([]string, 0, len(kept))
	for _, p := range kept {
		if p.IsSingleIP() {
			out = append(out, p.Addr().String())
		} else {
			out = append(out, p.String())
		}
	}
	return out
}
