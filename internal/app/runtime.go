package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akastrmix/akastr-agent/internal/desired"
	"github.com/akastrmix/akastr-agent/internal/layout"
	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/operation"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const recentOperationLimit = 64

// Runtime is the running set of enabled modules. The control connection only
// moves messages; every command, target, report and acknowledgement goes to a
// module.
type Runtime struct {
	operations *operation.Executor
	commands   map[string]module.Commands
	desired    *desired.Engine
	reporters  []module.Reporter
	workers    []module.Worker
}

// BuildRuntime validates every local dependency the enabled modules need. It
// only reads durable state, so update candidates may call it before activation.
func BuildRuntime(model *Model, paths layout.Layout) (*Runtime, error) {
	journal, err := operation.Open(operation.Options{StateFile: paths.StateFile(), RecentLimit: recentOperationLimit})
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{
		operations: operation.NewExecutor(journal),
		commands:   map[string]module.Commands{}, desired: desired.New(),
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

// PutState and SetStateKeys pass Cloud's targets to the Desired modules.
func (r *Runtime) PutState(put protocol.StatePut) error       { return r.desired.Put(put) }
func (r *Runtime) SetStateKeys(keys protocol.StateKeys) error { return r.desired.Keys(keys) }

// Run applies Desired modules and keeps every reporter and worker running; one
// stopping stops the process, so a node is never online while silently no
// longer observing.
func (r *Runtime) Run(ctx context.Context, publish module.Publish, status desired.Send) error {
	runContext, cancel := context.WithCancel(ctx)
	applied := make(chan struct{})
	go func() {
		defer close(applied)
		r.desired.Run(runContext, status)
	}()
	defer func() {
		cancel()
		<-applied
	}()
	tasks := make([]func(context.Context) error, 0, len(r.reporters)+len(r.workers))
	for _, reporter := range r.reporters {
		tasks = append(tasks, func(ctx context.Context) error { return reporter.Run(ctx, publish) })
	}
	for _, worker := range r.workers {
		tasks = append(tasks, worker.Run)
	}
	if len(tasks) == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan error, len(tasks))
	for _, task := range tasks {
		go func() { done <- task(runContext) }()
	}
	err := <-done
	cancel()
	for range len(tasks) - 1 {
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
	r.desired.ControlReady()
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

// UpdateSafe refuses to replace the process while module work must finish in
// this process first. A running command holds the lifecycle gate instead; one
// an earlier process left in the journal any version recovers alike.
func (r *Runtime) UpdateSafe() error {
	for _, reporter := range r.reporters {
		if err := reporter.UpdateSafe(); err != nil {
			return err
		}
	}
	return nil
}
