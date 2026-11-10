package firewall

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"
	"strings"
)

// Bounds of one target. The largest target stays far inside protocol.MaxMessage.
const (
	maxSets         = 64
	maxAddresses    = 4096
	maxDomains      = 64
	maxRules        = 512
	maxCeilings     = 256
	maxRanges       = 64
	maxRuleSources  = 64
	maxDomainLength = 253
)

const (
	modeOff     = "off"
	modeObserve = "observe"
	modeEnforce = "enforce"
)

var (
	setName     = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	domainLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	errInvalid  = errors.New("firewall target is invalid")
)

// Target is the node's whole inbound filter as Cloud computes it. Cloud sends
// facts and choices, never rule text; the skeleton and its order are the
// Agent's (see render.go).
type Target struct {
	Mode     string                `json:"mode"`
	Retry    int64                 `json:"retry"`
	Sets     map[string]AddressSet `json:"sets"`
	Blocked  *string               `json:"blocked"`
	Exempt   *string               `json:"exempt"`
	Ceilings []Ceiling             `json:"ceilings"`
	Rules    []Rule                `json:"rules"`
}

// AddressSet is a named group of sources. An empty set matches nobody.
type AddressSet struct {
	Addresses []string `json:"addresses"`
	Domains   []string `json:"domains"`
}

// Ceiling drops packets to its ports from anyone outside its sources.
type Ceiling struct {
	Protocol string   `json:"protocol"`
	Ports    [][2]int `json:"ports"`
	Sources  []string `json:"sources"`
}

// Rule is one ordered allow or drop; the first rule that matches decides.
type Rule struct {
	Action   string   `json:"action"`
	Sources  Sources  `json:"sources"`
	Protocol string   `json:"protocol"`
	Ports    [][2]int `json:"ports"`
}

// Sources is "any" or a non-empty list of set names. An empty list is a
// malformed target, never "everybody".
type Sources struct {
	Any  bool
	Sets []string
}

func (s Sources) MarshalJSON() ([]byte, error) {
	if s.Any {
		return []byte(`"any"`), nil
	}
	return json.Marshal(s.Sets)
}

func (s *Sources) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte(`"any"`)) {
		*s = Sources{Any: true}
		return nil
	}
	var sets []string
	if err := json.Unmarshal(data, &sets); err != nil || len(sets) == 0 {
		return errInvalid
	}
	*s = Sources{Sets: sets}
	return nil
}

// off is the target of a node whose key is gone: no table.
func off() Target {
	return Target{Mode: modeOff, Sets: map[string]AddressSet{}, Ceilings: []Ceiling{}, Rules: []Rule{}}
}

var targetFields = []string{"mode", "retry", "sets", "blocked", "exempt", "ceilings", "rules"}

// decodeTarget accepts exactly the contract's fields, every one present;
// only blocked and exempt may be null.
func decodeTarget(raw []byte) (Target, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || len(object) != len(targetFields) {
		return Target{}, errInvalid
	}
	for _, field := range targetFields {
		value, found := object[field]
		if !found || (field != "blocked" && field != "exempt" && bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
			return Target{}, errInvalid
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var t Target
	if err := decoder.Decode(&t); err != nil || decoder.More() {
		return Target{}, errInvalid
	}
	// Every nested object must carry all of its fields, so a missing list
	// never reads as an empty one.
	if err := requireNested(object); err != nil {
		return Target{}, err
	}
	if err := t.validate(); err != nil {
		return Target{}, err
	}
	return t, nil
}

func requireNested(object map[string]json.RawMessage) error {
	check := func(raw json.RawMessage, fields ...string) error {
		var items map[string]json.RawMessage
		if json.Unmarshal(raw, &items) != nil || len(items) != len(fields) {
			return errInvalid
		}
		for _, field := range fields {
			if value, found := items[field]; !found || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return errInvalid
			}
		}
		return nil
	}
	var sets map[string]json.RawMessage
	if json.Unmarshal(object["sets"], &sets) != nil {
		return errInvalid
	}
	for _, raw := range sets {
		if err := check(raw, "addresses", "domains"); err != nil {
			return err
		}
	}
	for field, fields := range map[string][]string{
		"ceilings": {"protocol", "ports", "sources"},
		"rules":    {"action", "sources", "protocol", "ports"},
	} {
		var items []json.RawMessage
		if json.Unmarshal(object[field], &items) != nil {
			return errInvalid
		}
		for _, raw := range items {
			if err := check(raw, fields...); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t Target) validate() error {
	if t.Mode != modeOff && t.Mode != modeObserve && t.Mode != modeEnforce {
		return errInvalid
	}
	if t.Retry < 0 || t.Retry > 1<<53-1 || len(t.Sets) > maxSets || len(t.Ceilings) > maxCeilings || len(t.Rules) > maxRules {
		return errInvalid
	}
	addresses, domains := 0, 0
	for name, set := range t.Sets {
		if !setName.MatchString(name) || set.Addresses == nil || set.Domains == nil {
			return errInvalid
		}
		addresses += len(set.Addresses)
		domains += len(set.Domains)
		for _, address := range set.Addresses {
			if _, err := parsePrefix(address); err != nil {
				return err
			}
		}
		for _, domain := range set.Domains {
			if !validDomain(domain) {
				return errInvalid
			}
		}
	}
	if addresses > maxAddresses || domains > maxDomains {
		return errInvalid
	}
	known := func(name string) bool { _, found := t.Sets[name]; return found }
	for _, name := range []*string{t.Blocked, t.Exempt} {
		if name != nil && !known(*name) {
			return errInvalid
		}
	}
	names := func(sets []string) bool {
		if len(sets) == 0 || len(sets) > maxRuleSources {
			return false
		}
		for _, name := range sets {
			if !known(name) {
				return false
			}
		}
		return true
	}
	for _, c := range t.Ceilings {
		if (c.Protocol != "tcp" && c.Protocol != "udp") || !validRanges(c.Ports, false) || !names(c.Sources) {
			return errInvalid
		}
	}
	for _, r := range t.Rules {
		if r.Action != "allow" && r.Action != "drop" {
			return errInvalid
		}
		if !r.Sources.Any && !names(r.Sources.Sets) {
			return errInvalid
		}
		switch r.Protocol {
		case "tcp", "udp":
			if !validRanges(r.Ports, false) {
				return errInvalid
			}
		case "ping", "any":
			if !validRanges(r.Ports, true) {
				return errInvalid
			}
		default:
			return errInvalid
		}
	}
	return nil
}

func validRanges(ports [][2]int, empty bool) bool {
	if ports == nil || (len(ports) == 0) != empty || len(ports) > maxRanges {
		return false
	}
	for _, p := range ports {
		if p[0] < 1 || p[0] > p[1] || p[1] > 65535 {
			return false
		}
	}
	return true
}

// parsePrefix reads an address or a network in its canonical form only, so
// one source has one spelling on both sides.
func parsePrefix(text string) (netip.Prefix, error) {
	if !strings.Contains(text, "/") {
		address, err := netip.ParseAddr(text)
		if err != nil || address.Zone() != "" || address.Is4In6() || address.String() != text {
			return netip.Prefix{}, errInvalid
		}
		return netip.PrefixFrom(address, address.BitLen()), nil
	}
	prefix, err := netip.ParsePrefix(text)
	if err != nil || prefix.Addr().Is4In6() || prefix.Masked() != prefix || prefix.String() != text {
		return netip.Prefix{}, errInvalid
	}
	return prefix, nil
}

func validDomain(name string) bool {
	if len(name) > maxDomainLength || !strings.Contains(name, ".") {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !domainLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// domains lists every domain the target names, once each.
func (t Target) domains() []string {
	seen := map[string]bool{}
	var names []string
	for _, set := range t.Sets {
		for _, domain := range set.Domains {
			if !seen[domain] {
				seen[domain] = true
				names = append(names, domain)
			}
		}
	}
	return names
}
