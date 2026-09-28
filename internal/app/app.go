package app

import (
	"context"
	"fmt"
	"net/http"

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/config"
	"github.com/akastrmix/akastr-agent/internal/layout"
)

// Model is a validated configuration: the node envelope and its enabled modules.
type Model struct {
	Config       config.Config
	Capabilities *capability.Registry
	modules      modules
}

func Load(configPath string) (*Model, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	return NewModel(cfg)
}

func NewModel(cfg config.Config) (*Model, error) {
	parsed, err := parseModules(cfg.Modules)
	if err != nil {
		return nil, err
	}
	registry, err := capability.New(parsed.capabilities()...)
	if err != nil {
		return nil, fmt.Errorf("build capability registry: %w", err)
	}
	return &Model{Config: cfg, Capabilities: registry, modules: parsed}, nil
}

// HostRequirements lists the commands the enabled modules need and the Debian
// packages that provide them.
func (m *Model) HostRequirements() (commands, packages []string) {
	return m.modules.hostCommands()
}

// Prepare fetches pinned module assets and checks that this binary can run the
// configuration here. Installers and update candidates run it before switching.
func Prepare(ctx context.Context, model *Model, paths layout.Layout, client *http.Client) error {
	if err := model.modules.prepare(ctx, paths, client); err != nil {
		return err
	}
	if _, err := BuildRuntime(model, paths); err != nil {
		return fmt.Errorf("validate runtime dependencies: %w", err)
	}
	return nil
}
