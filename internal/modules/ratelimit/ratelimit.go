// Package ratelimit limits the download of the users Cloud names on this
// node. Cloud decides who and how fast; the module only finds each user's
// connections in the Xray access log and polices them on the way out.
//
// Cloud limits groups of users: the clients of one subscription share one
// limit, whichever of them a connection uses. On every interface that carries
// a default route the module keeps a clsact egress hook with flower filters
// matching each connection's client address and port, TCP and UDP alike, and
// all filters of a group share one police action at the group's rate. The root qdisc and other filters stay as they
// are, so nobody unlimited is touched. What the module adds is marked by two
// filter priorities and a range of action indexes. It removes leftovers of an
// earlier process when it starts, everything when it stops, and everything on
// any failure: an unlimited user is better than a broken network.
package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/modules/xui"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const (
	Name         = "rate_limit"
	maxMbps      = 10000
	maxEmails    = 64
	syncInterval = 2 * time.Second
	stopTimeout  = 15 * time.Second

	codeLogUnavailable = "rate_limit_log_unavailable"
	codeApplyFailed    = "rate_limit_apply_failed"
)

// Config has no fields: the module is on or off.
type Config struct{}

func ParseConfig(raw json.RawMessage) (Config, error) {
	return protocol.DecodeStrict[Config](raw, Name)
}

// Target is the shared download limit of one group of owned clients, keyed by
// Cloud's group. Zero means none, which Cloud sends before it retires the key.
type Target struct {
	DownMbps int      `json:"down_mbps"`
	Emails   []string `json:"emails"`
}

func decodeTarget(raw json.RawMessage) (Target, error) {
	t, err := protocol.DecodeStrict[Target](raw, "rate_limit target", "down_mbps", "emails")
	invalid := err != nil || t.DownMbps < 0 || t.DownMbps > maxMbps || len(t.Emails) > maxEmails
	seen := map[string]bool{}
	for _, email := range t.Emails {
		invalid = invalid || !xui.Owned(email) || seen[email]
		seen[email] = true
	}
	if invalid {
		return Target{}, errors.New("rate_limit target is invalid")
	}
	return t, nil
}

// Connections is what the module needs from the access log.
type Connections interface {
	Err() error
	Connections(emails map[string]bool) map[string][]netip.AddrPort
}

type Limiter struct {
	log    Connections
	tc     runner
	routes func() (v4, v6 []string, err error)
	links  func() ([]string, error)

	mu     sync.Mutex
	limits map[string]Target
	// clean is set once the leftovers of an earlier process are gone.
	clean bool
	rules *rules
	code  string
}

var (
	_ module.Desired = (*Limiter)(nil)
	_ module.Worker  = (*Limiter)(nil)
)

func New(log Connections) *Limiter {
	return &Limiter{log: log, tc: execTC{}, links: links, limits: map[string]Target{}, rules: newRules(),
		routes: func() ([]string, []string, error) { return defaultRoutes("/proc") }}
}

func (l *Limiter) Validate(_ string, raw json.RawMessage) error {
	_, err := decodeTarget(raw)
	return err
}

// Apply takes Cloud's complete set of limits and installs it at once; Run
// keeps it current as connections come and go.
func (l *Limiter) Apply(ctx context.Context, targets map[string]json.RawMessage) (map[string]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	limits := map[string]Target{}
	for key, raw := range targets {
		if raw == nil {
			// Unusable target: keep whatever this group had.
			if t, found := l.limits[key]; found {
				limits[key] = t
			}
			continue
		}
		if t, err := decodeTarget(raw); err == nil && t.DownMbps > 0 && len(t.Emails) > 0 {
			limits[key] = t
		}
	}
	l.limits = limits
	// Cloud's periodic reapply is the moment to repair rules changed by hand.
	if l.clean && l.rules.drifted(ctx, l.tc) {
		l.clean = false
	}
	if err := l.sync(ctx); err != nil {
		return nil, err
	}
	codes := map[string]string{}
	if l.code != "" {
		for key := range l.limits {
			codes[key] = l.code
		}
	}
	return codes, nil
}

// Run clears what an earlier process left, then follows the users'
// connections until ctx ends and removes everything on the way out.
func (l *Limiter) Run(ctx context.Context) error {
	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()
	for {
		l.mu.Lock()
		// After a failure the rules are gone and Apply retries with backoff.
		if l.code != codeApplyFailed {
			if err := l.sync(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("rate limit rules could not be removed", "error", err.Error())
			}
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			stop, cancel := context.WithTimeout(context.Background(), stopTimeout)
			defer cancel()
			l.mu.Lock()
			defer l.mu.Unlock()
			if err := l.removeAll(stop); err != nil {
				slog.Warn("rate limit rules remain after stop", "error", err.Error())
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// sync converges the installed rules to the limits and the users' current
// connections. On failure it removes everything and records why; it returns
// an error only when that removal failed too. l.mu must be held.
func (l *Limiter) sync(ctx context.Context) error {
	if !l.clean {
		if err := l.removeAll(ctx); err != nil {
			l.setCode(codeApplyFailed)
			return err
		}
		l.clean = true
	}
	want, code := l.want()
	if code == codeApplyFailed {
		l.setCode(code)
		return l.removeAll(ctx)
	}
	if err := l.rules.converge(ctx, l.tc, want); err != nil {
		l.setCode(codeApplyFailed)
		slog.Warn("rate limit rules failed; removing them", "error", err.Error())
		return l.removeAll(ctx)
	}
	l.setCode(code)
	return nil
}

// want is the rule set for the current limits. A log that cannot be read
// keeps the connections already known and only reports itself.
func (l *Limiter) want() (desired, string) {
	if len(l.limits) == 0 {
		return desired{}, ""
	}
	v4, v6, err := l.routes()
	if err != nil || len(v4)+len(v6) == 0 {
		return desired{}, codeApplyFailed
	}
	emails := map[string]bool{}
	for _, t := range l.limits {
		for _, email := range t.Emails {
			emails[email] = true
		}
	}
	code := ""
	if l.log.Err() != nil {
		code = codeLogUnavailable
	}
	return plan(l.limits, l.log.Connections(emails), v4, v6), code
}

func (l *Limiter) removeAll(ctx context.Context) error {
	l.rules = newRules()
	l.clean = false
	devs, err := l.links()
	if err == nil {
		err = cleanup(ctx, l.tc, devs)
	}
	if err != nil {
		return err
	}
	l.clean = true
	return nil
}

func (l *Limiter) setCode(code string) {
	if code != l.code {
		if code != "" {
			slog.Warn("rate limit not fully applied", "code", code)
		}
		l.code = code
	}
}

// Leftovers removes what a process that ran the module left behind, for a
// node whose module is now off: an update replaces the process without
// stopping it, and a crash never stops it.
type Leftovers struct{}

var _ module.Worker = Leftovers{}

func (Leftovers) Run(ctx context.Context) error {
	devs, err := links()
	if err == nil {
		err = cleanup(ctx, execTC{}, devs)
	}
	if err != nil && ctx.Err() == nil {
		slog.Warn("rate limit rules of an earlier process remain", "error", err.Error())
	}
	<-ctx.Done()
	return ctx.Err()
}
