package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type Handler interface {
	Run(context.Context, protocol.OperationOffer) protocol.ExecutionResult
	Recover(protocol.OperationOffer) protocol.ExecutionResult
	// Failed is the command type's failed result for an offer that never ran.
	Failed(offer protocol.OperationOffer, code string) protocol.ExecutionResult
}

// Executor is the single owner of live execution, journal recovery and terminal
// persistence. Duplicate callers join an existing execution instead of treating
// its active journal as a crashed process.
type Executor struct {
	engine  *Engine
	mu      sync.Mutex
	running map[string]*execution
}

type execution struct {
	done chan struct{}
	err  error
}

func NewExecutor(engine *Engine) *Executor {
	return &Executor{engine: engine, running: make(map[string]*execution)}
}

func (e *Executor) Execute(ctx context.Context, offer protocol.OperationOffer, group string, handler Handler) (protocol.ExecutionResult, error) {
	e.mu.Lock()
	if running := e.running[offer.CommandID]; running != nil {
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return protocol.ExecutionResult{}, ctx.Err()
		case <-running.done:
			if running.err != nil {
				return protocol.ExecutionResult{}, running.err
			}
			return e.replay(offer.CommandID)
		}
	}
	running := &execution{done: make(chan struct{})}
	e.running[offer.CommandID] = running
	e.mu.Unlock()
	result, err := e.execute(ctx, offer, group, handler)
	e.mu.Lock()
	running.err = err
	delete(e.running, offer.CommandID)
	close(running.done)
	e.mu.Unlock()
	return result, err
}

func (e *Executor) replay(id string) (protocol.ExecutionResult, error) {
	record, found := e.engine.Recent(id)
	if !found || len(record.TerminalResult) == 0 {
		return protocol.ExecutionResult{}, errors.New("persisted operation result is missing")
	}
	var result protocol.ExecutionResult
	if err := json.Unmarshal(record.TerminalResult, &result); err != nil {
		return result, fmt.Errorf("decode persisted operation result: %w", err)
	}
	return result, nil
}

func (e *Executor) execute(ctx context.Context, offer protocol.OperationOffer, group string, handler Handler) (protocol.ExecutionResult, error) {
	if _, found := e.engine.Recent(offer.CommandID); found {
		return e.replay(offer.CommandID)
	}
	var result protocol.ExecutionResult
	_, err := e.engine.Begin(offer.CommandID, offer.CommandType, group)
	if errors.Is(err, ErrConflict) && e.finishLeftover(offer, group, handler) {
		_, err = e.engine.Begin(offer.CommandID, offer.CommandType, group)
	}
	if err != nil {
		active, found := e.engine.Active(offer.CommandID)
		// Another command of this process holds the group, which happens only after Cloud
		// gave up on an accepted one. This one does not run; nothing is journaled, so
		// Cloud's next offer of it, if any, is decided afresh.
		if !found && errors.Is(err, ErrConflict) {
			return handler.Failed(offer, "operation_busy"), nil
		}
		if !found || active.Kind != offer.CommandType || active.ExclusiveGroup != group {
			return result, fmt.Errorf("begin operation: %w", err)
		}
		result = handler.Recover(offer)
	} else {
		result = handler.Run(ctx, offer)
	}
	if err := e.finish(offer.CommandID, result); err != nil {
		return protocol.ExecutionResult{}, err
	}
	return result, nil
}

// finishLeftover finishes the command holding the group when an earlier process left
// it running. Cloud offers another command of the group only after giving up on that
// one, and never offers it again, so nothing else would finish it: it ends as its
// module recovers one, never run again. It reports whether the group is free.
func (e *Executor) finishLeftover(offer protocol.OperationOffer, group string, handler Handler) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	left, found := e.engine.Snapshot().Active[group]
	if _, live := e.running[left.OperationID]; !found || live || left.Kind != offer.CommandType {
		return false
	}
	return e.finish(left.OperationID, handler.Recover(protocol.OperationOffer{CommandID: left.OperationID, CommandType: left.Kind})) == nil
}

func (e *Executor) finish(id string, result protocol.ExecutionResult) error {
	persisted, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode operation result: %w", err)
	}
	status := StatusFailed
	if result.Outcome == "succeeded" {
		status = StatusSucceeded
	} else if result.Outcome == "cancelled" {
		status = StatusCancelled
	}
	if _, err := e.engine.FinishWithResult(id, status, result.Code, persisted); err != nil {
		return fmt.Errorf("persist operation result: %w", err)
	}
	return nil
}
