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

// Without the final drop this records what the default would catch.
const finalRecord = tcpConnecting + ` update @h_final { meta l4proto . th dport counter }
meta l4proto udp update @h_final { meta l4proto . th dport counter }
meta l4proto icmp update @h_icmp4 { icmp type counter }
meta l4proto ipv6-icmp update @h_icmp6 { icmpv6 type counter }
meta l4proto != { tcp, udp, icmp, ipv6-icmp } update @h_proto { meta l4proto counter }`

// rendered is one target as nftables sees it: the rules, whose digest decides
// whether the installed table must be replaced, and the set members, which
// change without replacing it.
type rendered struct {
	mode    string
	digest  string
	rules   string
	members map[string][]string
}

// render turns a target into rules. resolved holds each domain's last
// addresses; a domain without any adds nobody.
func render(t Target, resolved map[string][]netip.Addr) rendered {
	r := rendered{mode: t.Mode, members: members(t, resolved)}
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
	if !found || (mode != modeObserve && mode != modeEnforce) || len(digest) != digestLength {
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
	return remove() + r.rules + r.memberScript(false)
}

// syncMembers replaces every set's members in one transaction.
func (r rendered) syncMembers() string { return r.memberScript(true) }

func (r rendered) memberScript(flush bool) string {
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(r.members)) {
		if flush {
			fmt.Fprintf(&b, "flush set inet %s %s\n", Table, name)
		}
		if elements := r.members[name]; len(elements) > 0 {
			fmt.Fprintf(&b, "add element inet %s %s { %s }\n", Table, name, strings.Join(elements, ", "))
		}
	}
	return b.String()
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
	b.WriteString("  chain input {\n")
	line("type filter hook input priority filter; policy accept;")
	if mark == "" {
		line(`iif "lo" accept`)
	} else {
		line(`iif "lo" accept comment %q`, mark)
	}
	if t.Blocked != nil {
		for _, f := range families {
			exempt := ""
			if t.Exempt != nil {
				exempt = fmt.Sprintf(" %s saddr != @%s", f.ip, memberSet(*t.Exempt, f.number))
			}
			line("ct direction original %s saddr @%s%s drop", f.ip, memberSet(*t.Blocked, f.number), exempt)
		}
	}
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
