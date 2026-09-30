package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akastrmix/akastr-agent/internal/layout"
	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/operation"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const recentOperationLimit = 64

// Runtime is the running set of enabled modules. The control connection only
// moves messages; every command, report and acknowledgement goes to a module.
type Runtime struct {
	journal    *operation.Engine
	operations *operation.Executor
	commands   map[string]module.Commands
	reporters  []module.Reporter
}

// BuildRuntime validates every local dependency the enabled modules need. It
// only reads durable state, so update candidates may call it before activation.
func BuildRuntime(model *Model, paths layout.Layout) (*Runtime, error) {
	journal, err := operation.Open(operation.Options{StateFile: paths.StateFile(), RecentLimit: recentOperationLimit})
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{
		journal: journal, operations: operation.NewExecutor(journal),
		commands: map[string]module.Commands{},
	}
	if err := model.modules.build(paths, runtime); err != nil {
		return nil, err
	}
	return runtime, nil
}

func (r *Runtime) addCommands(commands module.Commands) {
	r.commands[commands.CommandType()] = commands
}

// Accepting validates an offer and reports whether its module takes new work now.
func (r *Runtime) Accepting(offer protocol.OperationOffer) (bool, error) {
	commands, found := r.commands[offer.CommandType]
	if !found {
		return false, fmt.Errorf("command type %q is not enabled on this node", offer.CommandType)
	}
	if err := commands.Validate(offer.Payload); err != nil {
		return false, err
	}
	return commands.Accepting(), nil
}

func (r *Runtime) Execute(ctx context.Context, offer protocol.OperationOffer) (protocol.ExecutionResult, error) {
	commands, found := r.commands[offer.CommandType]
	if !found {
		return protocol.ExecutionResult{}, errors.New("accepted command has no enabled module")
	}
	return r.operations.Execute(ctx, offer, commands.ExclusiveGroup(), commands)
}

// Run keeps every reporter running; one stopping stops the process, so a node
// is never online while silently no longer observing.
func (r *Runtime) Run(ctx context.Context, publish module.Publish) error {
	if len(r.reporters) == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, len(r.reporters))
	for _, reporter := range r.reporters {
		go func() { done <- reporter.Run(runContext, publish) }()
	}
	err := <-done
	cancel()
	for range len(r.reporters) - 1 {
		<-done
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		err = errors.New("module stopped unexpectedly")
	}
	return err
}

func (r *Runtime) ControlReady() {
	for _, reporter := range r.reporters {
		reporter.ControlReady()
	}
}

// Acknowledge passes Cloud's acknowledgement to the module that sent the
// report. One no module owns is a repeat of an earlier acknowledgement.
func (r *Runtime) Acknowledge(reportID string) error {
	for _, reporter := range r.reporters {
		if handled, err := reporter.Acknowledge(reportID); handled {
			return err
		}
	}
	return nil
}

// UpdateSafe refuses to replace the process while an operation or module work
// must finish in this process first.
func (r *Runtime) UpdateSafe() error {
	if len(r.journal.Snapshot().Active) != 0 {
		return errors.New("an Agent operation is active")
	}
	for _, reporter := range r.reporters {
		if err := reporter.UpdateSafe(); err != nil {
			return err
		}
	}
	return nil
}
