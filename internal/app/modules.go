package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"path/filepath"
	"time"

	"github.com/akastrmix/akastr-agent/internal/layout"
	"github.com/akastrmix/akastr-agent/internal/modules/changeip"
	"github.com/akastrmix/akastr-agent/internal/modules/connectionrecords"
	"github.com/akastrmix/akastr-agent/internal/modules/diagnostics"
	"github.com/akastrmix/akastr-agent/internal/modules/firewall"
	"github.com/akastrmix/akastr-agent/internal/modules/ipqualityrunner"
	"github.com/akastrmix/akastr-agent/internal/modules/ipwatch"
	"github.com/akastrmix/akastr-agent/internal/modules/metrics"
	"github.com/akastrmix/akastr-agent/internal/modules/ratelimit"
	"github.com/akastrmix/akastr-agent/internal/modules/routing"
	"github.com/akastrmix/akastr-agent/internal/modules/socks5"
	"github.com/akastrmix/akastr-agent/internal/modules/xui"
	"github.com/akastrmix/akastr-agent/internal/panel"
	"github.com/akastrmix/akastr-agent/internal/xray/accesslog"
	"github.com/akastrmix/akastr-agent/internal/xray/ledger"
)

// modules holds the parsed configuration of every module a node can enable.
// Adding a capability means one package under internal/modules plus one
// field and its cases in this file.
type modules struct {
	ipWatch     *ipwatch.Config
	changeIP    *changeip.Config
	socks5      *socks5.Config
	runner      *ipqualityrunner.Config
	xui         *xui.Config
	rateLimit   *ratelimit.Config
	records     *connectionrecords.Config
	routing     *routing.Config
	diagnostics *diagnostics.Config
	firewall    *firewall.Config
	metrics     *metrics.Config
	// controlEndpoint is where the firewall checks that Cloud stays reachable.
	controlEndpoint string
}

func parseModules(sections map[string]json.RawMessage) (modules, error) {
	var m modules
	for name, raw := range sections {
		var err error
		switch name {
		case xui.Name:
			m.xui, err = parseSection(xui.ParseConfig, raw)
		case ipwatch.Name:
			m.ipWatch, err = parseSection(ipwatch.ParseConfig, raw)
		case changeip.Name:
			m.changeIP, err = parseSection(changeip.ParseConfig, raw)
		case socks5.Name:
			m.socks5, err = parseSection(socks5.ParseConfig, raw)
		case ipqualityrunner.Name:
			m.runner, err = parseSection(ipqualityrunner.ParseConfig, raw)
		case ratelimit.Name:
			m.rateLimit, err = parseSection(ratelimit.ParseConfig, raw)
		case connectionrecords.Name:
			m.records, err = parseSection(connectionrecords.ParseConfig, raw)
		case routing.Name:
			m.routing, err = parseSection(routing.ParseConfig, raw)
		case diagnostics.Name:
			m.diagnostics, err = parseSection(diagnostics.ParseConfig, raw)
		case firewall.Name:
			m.firewall, err = parseSection(firewall.ParseConfig, raw)
		case metrics.Name:
			m.metrics, err = parseSection(metrics.ParseConfig, raw)
		default:
			return modules{}, fmt.Errorf("configuration module %q is unknown to this Agent", name)
		}
		if err != nil {
			return modules{}, fmt.Errorf("module %s: %w", name, err)
		}
	}
	// ChangeIP is confirmed only through the observed public IPv4.
	if m.changeIP != nil && m.ipWatch == nil {
		return modules{}, errors.New("module changeip requires ip_watch")
	}
	// Users and requests are found through the access log of the 3x-ui Xray.
	if m.rateLimit != nil && m.xui == nil {
		return modules{}, errors.New("module rate_limit requires xui")
	}
	if m.records != nil && m.xui == nil {
		return modules{}, errors.New("module connection_records requires xui")
	}
	// Routing is written into the 3x-ui panel's Xray template.
	if m.routing != nil && m.xui == nil {
		return modules{}, errors.New("module routing requires xui")
	}
	return m, nil
}

func parseSection[T any](parse func(json.RawMessage) (T, error), raw json.RawMessage) (*T, error) {
	cfg, err := parse(raw)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// hostCommands and hostPackages are what the installer provides with apt.
func (m modules) hostCommands() (commands, packages []string) {
	if m.runner != nil {
		commands = append(commands, ipqualityrunner.HostCommands...)
		packages = append(packages, ipqualityrunner.HostPackages...)
	}
	if m.rateLimit != nil {
		commands = append(commands, ratelimit.HostCommands...)
		packages = append(packages, ratelimit.HostPackages...)
	}
	if m.firewall != nil {
		commands = append(commands, firewall.HostCommands...)
		packages = append(packages, firewall.HostPackages...)
	}
	return commands, packages
}

// RemoveStaleAssets deletes pinned module assets that only earlier deployments
// used. Call it once this deployment is committed. A node with the Runner off
// may still hold an earlier pin's script.
func RemoveStaleAssets(paths layout.Layout) error {
	return ipqualityrunner.RemoveStaleScripts(paths.Root)
}

// prepare fetches pinned module assets before the runtime is built.
func (m modules) prepare(ctx context.Context, paths layout.Layout, client *http.Client) error {
	if m.runner != nil {
		return ipqualityrunner.PrepareScript(ctx, client, paths.Root)
	}
	return nil
}

func (m modules) build(paths layout.Layout, runtime *Runtime) error {
	if m.xui != nil {
		client := panel.New(m.xui.PanelURL, m.xui.Username, m.xui.Password)
		// The outbound check logs in on its own: a test takes seconds, and a
		// client makes one call at a time.
		checks := panel.New(m.xui.PanelURL, m.xui.Username, m.xui.Password)
		panel, err := xui.NewPanel(client, filepath.Join(paths.StateDir, xui.Name))
		if err != nil {
			return err
		}
		runtime.desired.Add(xui.Name, panel)
		if m.routing != nil {
			runtime.desired.Add(routing.Name, routing.New(client))
			runtime.reporters = append(runtime.reporters, routing.NewHealth(checks))
		}
		// The log is read once, and one account of the connections serves the
		// snapshot's connection counts, the limiter and the connection records.
		log := accesslog.New(panel.Inbounds)
		var lines ledger.Log = log
		var ended func(ledger.Ended)
		if m.records != nil {
			records := connectionrecords.New()
			lines, ended = sharedLog{log, records.Lines}, records.Ended
			runtime.reporters = append(runtime.reporters, records)
		}
		connections := ledger.New(lines, ended)
		runtime.workers = append(runtime.workers, connections)
		runtime.reporters = append(runtime.reporters, xui.NewSnapshots(panel, connections.Connections))
		if m.rateLimit != nil {
			limiter := ratelimit.New(connections)
			runtime.desired.Add(ratelimit.Name, limiter)
			runtime.workers = append(runtime.workers, limiter)
		}
	}
	if m.rateLimit == nil {
		runtime.workers = append(runtime.workers, ratelimit.Leftovers{})
	}
	if m.firewall != nil {
		filter, err := firewall.New(paths.StateDir, m.controlEndpoint)
		if err != nil {
			return err
		}
		runtime.desired.Add(firewall.Name, filter)
		runtime.workers = append(runtime.workers, filter)
		var panelPort *int
		if m.xui != nil {
			panelPort = m.xui.PanelPort()
		}
		runtime.reporters = append(runtime.reporters, firewall.NewStatus(filter, panelPort))
	} else {
		runtime.workers = append(runtime.workers, firewall.Leftovers{StateDir: paths.StateDir})
	}
	if m.metrics != nil {
		runtime.reporters = append(runtime.reporters, metrics.New())
	}
	sweeper := diagnostics.NewSweeper()
	runtime.workers = append(runtime.workers, sweeper)
	if m.diagnostics != nil {
		runtime.addQueries(diagnostics.New(sweeper))
	}
	if m.ipWatch != nil {
		observer := ipwatch.New()
		monitor, err := ipwatch.OpenMonitor(ipwatch.ReconciliationFile(paths.StateDir), observer,
			time.Duration(m.ipWatch.IntervalSeconds)*time.Second, m.ipWatch.IPv6)
		if err != nil {
			return err
		}
		runtime.reporters = append(runtime.reporters, ipwatch.Reporter{Monitor: monitor})
		if m.changeIP != nil {
			observe := func(ctx context.Context) (netip.Addr, time.Time, error) {
				observation, err := observer.Observe(ctx, ipwatch.IPv4)
				return observation.Address, observation.ObservedAt, err
			}
			change, err := changeip.New(*m.changeIP, observe, monitor)
			if err != nil {
				return err
			}
			runtime.addCommands(change)
		}
	}
	if m.runner != nil {
		runner, err := ipqualityrunner.New(paths.Root)
		if err != nil {
			return err
		}
		runtime.addCommands(runner)
	}
	return nil
}

// sharedLog passes the access log's lines to the ledger, then to also.
type sharedLog struct {
	*accesslog.Log
	also func([]accesslog.Line)
}

func (s sharedLog) Run(ctx context.Context, emit func([]accesslog.Line)) error {
	return s.Log.Run(ctx, func(lines []accesslog.Line) {
		emit(lines)
		s.also(lines)
	})
}
