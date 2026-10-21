package ratelimit

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"github.com/akastrmix/akastr-agent/internal/xray/ledger"
)

const (
	// Filter priorities and police indexes that mark the module's own rules.
	prefIPv4   = 49001
	prefIPv6   = 49002
	actionBase = 0xAC000000
	maxUsers   = 4096
	// Beyond this many filters further connections stay unlimited.
	maxFilters = 16384
)

// rule is one flower filter: one connection or UDP flow on one interface.
type rule struct {
	dev           string
	proto         string
	local, remote netip.AddrPort
}

// desired is the rule set for some limits: each group's rate and the group
// each rule polices.
type desired struct {
	rates map[string]int
	rules map[rule]string
}

// plan matches every connection of the limited groups, IPv4 on the IPv4
// default interfaces and IPv6 on the IPv6 ones, until maxFilters. A client
// that Cloud names in two groups follows the lower limit.
func plan(limits map[string]Target, flows map[string][]ledger.Flow, v4, v6 []string) desired {
	want := desired{rates: map[string]int{}, rules: map[rule]string{}}
	groups := slices.Sorted(maps.Keys(limits))
	owner := map[string]string{}
	for _, group := range groups {
		for _, email := range limits[group].Emails {
			if current, found := owner[email]; !found || limits[group].DownMbps < limits[current].DownMbps {
				owner[email] = group
			}
		}
	}
	for _, group := range groups {
		want.rates[group] = limits[group].DownMbps
		for _, email := range slices.Sorted(slices.Values(limits[group].Emails)) {
			if owner[email] != group {
				continue
			}
			fs := slices.Clone(flows[email])
			slices.SortFunc(fs, func(a, b ledger.Flow) int {
				return cmp.Or(a.Remote.Compare(b.Remote), a.Local.Compare(b.Local), strings.Compare(a.Proto, b.Proto))
			})
			for _, f := range fs {
				devs := v4
				if f.Remote.Addr().Is6() {
					devs = v6
				}
				for _, dev := range devs {
					if len(want.rules) >= maxFilters {
						return want
					}
					want.rules[rule{dev, f.Proto, f.Local, f.Remote}] = group
				}
			}
		}
	}
	return want
}

// rules is what the module installed, as far as it knows.
type rules struct {
	clsact  map[string]bool
	actions map[string]uint32 // group → police index
	rates   map[uint32]int
	filters map[rule]installed
	// orphans are police actions whose filters are gone; the kernel releases
	// them a moment later, so they are deleted on a later pass.
	orphans    map[uint32]bool
	nextHandle uint32
}

type installed struct {
	handle uint32
	index  uint32
}

func newRules() *rules {
	return &rules{clsact: map[string]bool{}, actions: map[string]uint32{}, rates: map[uint32]int{},
		filters: map[rule]installed{}, orphans: map[uint32]bool{}}
}

// converge changes the installed rules into want with one tc batch.
func (r *rules) converge(ctx context.Context, tc runner, want desired) error {
	for index := range r.orphans {
		if _, err := tc.run(ctx, "", "actions", "del", "action", "police", "index", fmt.Sprint(index)); err == nil {
			delete(r.orphans, index)
		}
	}
	index := r.assign(want)
	devs := map[string]bool{}
	for key, group := range want.rules {
		if index[group] != 0 {
			devs[key.dev] = true
		}
	}
	var batch []string
	for dev := range devs {
		if r.clsact[dev] {
			continue
		}
		out, err := tc.run(ctx, "", "qdisc", "show", "dev", dev)
		if err != nil {
			return fmt.Errorf("read qdiscs of %s: %w", dev, err)
		}
		if !strings.Contains(out, "clsact") {
			batch = append(batch, "qdisc add dev "+dev+" clsact")
		}
	}
	for group, i := range index {
		if r.actions[group] != i || r.rates[i] != want.rates[group] {
			batch = append(batch, fmt.Sprintf("actions replace action police rate %dmbit burst %d conform-exceed drop/continue index %d",
				want.rates[group], burst(want.rates[group]), i))
		}
	}
	for key, have := range r.filters {
		if index[want.rules[key]] != have.index {
			batch = append(batch, fmt.Sprintf("filter del dev %s egress protocol %s pref %d handle %d flower",
				key.dev, family(key.remote), pref(key.remote), have.handle))
		}
	}
	added := map[rule]installed{}
	keys := slices.SortedFunc(maps.Keys(want.rules), func(a, b rule) int {
		return cmp.Or(strings.Compare(a.dev, b.dev), a.remote.Compare(b.remote), a.local.Compare(b.local), strings.Compare(a.proto, b.proto))
	})
	for _, key := range keys {
		group := want.rules[key]
		i := index[group]
		if have, found := r.filters[key]; i == 0 || (found && have.index == i) {
			continue
		}
		r.nextHandle++
		added[key] = installed{r.nextHandle, i}
		batch = append(batch, fmt.Sprintf("filter add dev %s egress protocol %s pref %d handle %d flower %s action police index %d",
			key.dev, family(key.remote), pref(key.remote), r.nextHandle, key.match(), i))
	}
	if len(batch) > 0 {
		if _, err := tc.run(ctx, strings.Join(batch, "\n")+"\n", "-batch", "-"); err != nil {
			return err
		}
	}
	// The whole batch succeeded; record it.
	for dev := range devs {
		r.clsact[dev] = true
	}
	for key, have := range r.filters {
		if index[want.rules[key]] != have.index {
			delete(r.filters, key)
		}
	}
	maps.Copy(r.filters, added)
	for group, i := range r.actions {
		if index[group] != i {
			r.orphans[i] = true
			delete(r.rates, i)
		}
	}
	r.actions = index
	for group, i := range index {
		r.rates[i] = want.rates[group]
	}
	return nil
}

// drifted reports whether the filters on the interfaces differ from what
// the module installed, such as after someone removed the clsact hook.
func (r *rules) drifted(ctx context.Context, tc runner) bool {
	want := map[string]int{}
	for key := range r.filters {
		want[key.dev]++
	}
	for dev := range r.clsact {
		out, err := tc.run(ctx, "", "filter", "show", "dev", dev, "egress")
		if err != nil {
			return true
		}
		have := 0
		for _, line := range strings.Split(out, "\n") {
			if hasOwnFilter(line) && strings.Contains(line, "handle 0x") {
				have++
			}
		}
		if have != want[dev] {
			return true
		}
	}
	return false
}

// assign gives every limited group a police index: the one it has, or the
// lowest free one. Indexes being released are not reused in the same pass.
func (r *rules) assign(want desired) map[string]uint32 {
	index := map[string]uint32{}
	used := maps.Clone(r.orphans)
	var newcomers []string
	for group, i := range r.actions {
		used[i] = true
		if _, keep := want.rates[group]; keep {
			index[group] = i
		}
	}
	for group := range want.rates {
		if _, found := index[group]; !found {
			newcomers = append(newcomers, group)
		}
	}
	slices.Sort(newcomers)
	next := uint32(1)
	for _, group := range newcomers {
		for next <= maxUsers && used[actionBase+next] {
			next++
		}
		if next > maxUsers {
			break
		}
		index[group] = actionBase + next
		next++
	}
	return index
}

// match is the flower match of the rule's packets on the way to the client.
// A UDP flow's local address is unknown, so it matches by port only.
func (r rule) match() string {
	local := ""
	if r.local.Addr().IsValid() {
		local = "src_ip " + r.local.Addr().String() + " "
	}
	return fmt.Sprintf("ip_proto %s %ssrc_port %d dst_ip %s dst_port %d", r.proto, local, r.local.Port(), r.remote.Addr(), r.remote.Port())
}

func family(addr netip.AddrPort) string {
	if addr.Addr().Is6() {
		return "ipv6"
	}
	return "ip"
}

func pref(addr netip.AddrPort) int {
	if addr.Addr().Is6() {
		return prefIPv6
	}
	return prefIPv4
}

// burst lets about 100 ms of traffic through at once, at least 64 KiB.
func burst(mbps int) int {
	return max(mbps*1_000_000/8/10, 64<<10)
}
