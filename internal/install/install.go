// Package install converges a machine on the node described by one install
// command. It is safe to rerun: a failed or interrupted install is repaired by
// running the same command again.
package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/akastrmix/akastr-agent/internal/app"
	"github.com/akastrmix/akastr-agent/internal/bootstrap"
	"github.com/akastrmix/akastr-agent/internal/config"
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/layout"
)

const serviceName = "akastr-agent.service"

// System runs host commands (systemctl, apt-get).
type System func(ctx context.Context, name string, args ...string) error

type Options struct {
	AgentID           string
	MachineToken      string
	BootstrapEndpoint string
	Version           string
	// Executable is this installer binary; it becomes the installed release.
	Executable string
	Layout     layout.Layout
	UnitFile   string
	HTTPClient *http.Client
	System     System
	LookPath   func(string) (string, error)
	Output     io.Writer
}

func Install(ctx context.Context, o Options) error {
	lock, err := o.Layout.Lock()
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := checkOwnership(o.Layout, o.AgentID); err != nil {
		return err
	}
	raw, err := bootstrap.Fetch(ctx, o.HTTPClient, o.BootstrapEndpoint, o.AgentID, o.MachineToken)
	if err != nil {
		return err
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return err
	}
	if cfg.AgentID != o.AgentID {
		return errors.New("bootstrap configuration belongs to another node")
	}
	model, err := app.NewModel(cfg)
	if err != nil {
		return err
	}
	if err := installHostPackages(ctx, o, model); err != nil {
		return err
	}
	binary, err := os.ReadFile(o.Executable)
	if err != nil {
		return fmt.Errorf("read installer binary: %w", err)
	}
	for _, directory := range []string{o.Layout.StateDir, filepath.Dir(o.Layout.IdentityFile)} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
	}
	if err := app.Prepare(ctx, model, o.Layout, o.HTTPClient); err != nil {
		return err
	}
	slot, err := o.Layout.InactiveSlot()
	if err != nil {
		return err
	}
	if err := o.Layout.PrepareSlot(slot); err != nil {
		return err
	}
	if err := layout.WriteFile(layout.SlotBinary(slot), binary, 0o755); err != nil {
		return err
	}
	if err := layout.WriteFile(layout.SlotConfig(slot), raw, 0o600); err != nil {
		return err
	}
	if err := layout.SyncDirectory(slot); err != nil {
		return err
	}
	fresh, err := identity.Generate(o.AgentID)
	if err != nil {
		return err
	}
	// The running Agent keeps serving until Cloud accepts the new key; from then
	// on only this installation can authenticate.
	if err := fresh.Enroll(ctx, identity.Enrollment{
		ControlEndpoint: cfg.ControlEndpoint, MachineToken: o.MachineToken,
		AgentVersion: o.Version, ConfigurationRevision: cfg.ConfigurationRevision,
		Capabilities: model.Capabilities.List(), HTTPClient: o.HTTPClient,
	}); err != nil {
		return err
	}
	if _, err := os.Stat(o.UnitFile); err == nil {
		if err := o.System(ctx, "systemctl", "stop", serviceName); err != nil {
			return fmt.Errorf("stop the running Agent: %w", err)
		}
	}
	if err := fresh.Save(o.Layout.IdentityFile); err != nil {
		return err
	}
	if err := o.Layout.Activate(slot); err != nil {
		return err
	}
	if err := os.Remove(o.Layout.AttemptFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := layout.WriteFile(o.UnitFile, []byte(unit(o.Layout)), 0o644); err != nil {
		return err
	}
	_ = o.System(ctx, "systemctl", "reset-failed", serviceName)
	for _, command := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", serviceName},
		{"systemctl", "restart", serviceName},
	} {
		if err := o.System(ctx, command[0], command[1:]...); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(command, " "), err)
		}
	}
	_, err = fmt.Fprintf(o.Output, "Akastr Agent %s installed successfully.\n", o.Version)
	return err
}

// checkOwnership refuses to take over another node, or to inherit execution
// state whose node cannot be identified.
func checkOwnership(paths layout.Layout, agentID string) error {
	owner, found, err := identity.ReadAgentID(paths.IdentityFile)
	if err != nil {
		return fmt.Errorf("%w; uninstall the old Agent before installing", err)
	}
	if found {
		if owner != agentID {
			return errors.New("this machine runs another Agent node; uninstall it before installing")
		}
		return nil
	}
	for _, path := range []string{paths.StateFile(), paths.IPStateFile()} {
		if _, err := os.Stat(path); err == nil {
			return errors.New("existing Agent state has no identity; uninstall the old Agent before installing")
		}
	}
	return nil
}

// installHostPackages installs the Debian packages the enabled modules need
// when their commands are missing. Only the installer runs outside the sandbox.
func installHostPackages(ctx context.Context, o Options, model *app.Model) error {
	commands, packages := model.HostRequirements()
	lookPath := o.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	missing := func() string {
		for _, command := range commands {
			if _, err := lookPath(command); err != nil {
				return command
			}
		}
		return ""
	}
	if missing() == "" {
		return nil
	}
	if err := o.System(ctx, "apt-get", "update"); err != nil {
		return fmt.Errorf("apt-get update: %w", err)
	}
	if err := o.System(ctx, "apt-get", append([]string{"install", "-y", "--no-install-recommends"}, packages...)...); err != nil {
		return fmt.Errorf("install module packages: %w", err)
	}
	if command := missing(); command != "" {
		return fmt.Errorf("command %s is unavailable after package installation", command)
	}
	return nil
}

func unit(paths layout.Layout) string {
	current := paths.Current()
	return fmt.Sprintf(`[Unit]
Description=Akastr Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
NotifyAccess=main
ExecStart=%s run --config %s
Restart=always
RestartSec=5s
TimeoutStartSec=45s
TimeoutStopSec=30s
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=%s %s
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true

[Install]
WantedBy=multi-user.target
`, layout.SlotBinary(current), layout.SlotConfig(current), paths.StateDir, paths.Root)
}

// RunSystem runs a host command with its output shown to the operator.
func RunSystem(output io.Writer) System {
	return func(ctx context.Context, name string, args ...string) error {
		command := exec.CommandContext(ctx, name, args...)
		command.Stdout, command.Stderr = output, output
		command.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		return command.Run()
	}
}
