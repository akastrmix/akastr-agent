package firewall

import (
	"errors"
	"net/netip"
	"time"
)

const (
	stateSchema = 1
	// maxDomainAddresses bounds what one name may resolve to.
	maxDomainAddresses = 32
)

// persisted is StateDir/firewall/state.json: what the node restores without
// Cloud. Its size follows the target bounds: two targets and the last result
// of each domain they name.
type persisted struct {
	SchemaVersion int `json:"schema_version"`
	// Confirmed is the last target installed while Cloud stayed reachable.
	Confirmed *Target `json:"confirmed"`
	// Candidate is installed but not yet confirmed; a process that finds it
	// on start probes again before keeping it.
	Candidate *Target `json:"candidate"`
	// Failed names the target that cut the node off; it is not installed
	// again until Cloud changes it.
	Failed  string                  `json:"failed"`
	Domains map[string]domainResult `json:"domains"`
}

// domainResult is a domain's last addresses. A failed or empty lookup keeps
// them and only sets Failed.
type domainResult struct {
	Addresses  []string   `json:"addresses"`
	ResolvedAt *time.Time `json:"resolved_at"`
	Failed     bool       `json:"failed"`
}

func emptyState() persisted {
	return persisted{SchemaVersion: stateSchema, Domains: map[string]domainResult{}}
}

func (p persisted) validate() error {
	invalid := errors.New("firewall state file is invalid")
	if p.SchemaVersion != stateSchema || p.Domains == nil || len(p.Domains) > 3*maxDomains || len(p.Failed) > digestLength {
		return invalid
	}
	for _, t := range []*Target{p.Confirmed, p.Candidate} {
		if t != nil && t.validate() != nil {
			return invalid
		}
	}
	for name, result := range p.Domains {
		if !validDomain(name) || result.Addresses == nil || len(result.Addresses) > maxDomainAddresses {
			return invalid
		}
		for _, address := range result.Addresses {
			if parsed, err := netip.ParseAddr(address); err != nil || parsed.String() != address {
				return invalid
			}
		}
	}
	return nil
}

// resolved is each domain's addresses, for rendering.
func (p persisted) resolved() map[string][]netip.Addr {
	result := make(map[string][]netip.Addr, len(p.Domains))
	for name, domain := range p.Domains {
		for _, address := range domain.Addresses {
			if parsed, err := netip.ParseAddr(address); err == nil {
				result[name] = append(result[name], parsed)
			}
		}
	}
	return result
}

// prune keeps the results of the domains that the given targets name.
func (p *persisted) prune(targets ...*Target) {
	named := map[string]bool{}
	for _, t := range targets {
		if t != nil {
			for _, domain := range t.domains() {
				named[domain] = true
			}
		}
	}
	for name := range p.Domains {
		if !named[name] {
			delete(p.Domains, name)
		}
	}
}
