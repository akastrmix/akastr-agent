package ws

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type recordingExecutor struct {
	executed chan string
}

type blockingExecutor struct {
	started chan struct{}
	release chan struct{}
}

func (e *blockingExecutor) Execute(context.Context, protocol.OperationOffer) protocol.ExecutionResult {
	close(e.started)
	<-e.release
	return protocol.ExecutionResult{Outcome: "failed", Code: "test", Result: map[string]any{}}
}

type failingObservationSource struct{}

func (failingObservationSource) Run(context.Context, func(protocol.IPObservationBody) error) error {
	return errors.New("observation state failed")
}

func (failingObservationSource) Ack(string) error { return nil }

type nilObservationSource struct{}

func (*nilObservationSource) Run(context.Context, func(protocol.IPObservationBody) error) error {
	return nil
}

func TestOperationLeaseBlocksUpdateUntilExecutionFinishes(t *testing.T) {
	gate := lifecycle.New()
	executor := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	client := &Client{
		executor: executor, lifecycle: gate, running: map[string]*lifecycle.Lease{},
		pending: map[string]pendingOperation{}, completed: map[string]protocol.ExecutionResult{},
	}
	offer := protocol.OperationOffer{
		CommandID: "123e4567-e89b-42d3-a456-426614174002",
		NotBefore: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Minute),
	}
	lease, _ := gate.TryOperation()
	client.pending[offer.CommandID] = pendingOperation{offer: offer, lease: lease}
	client.handleAcceptedAck(t.Context(), protocol.AcceptedAckBody{CommandID: offer.CommandID, Accepted: true})
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("operation did not start")
	}
	if update, ok := gate.TryUpdate(); ok || update != nil {
		t.Fatal("update acquired while an accepted operation was executing")
	}
	close(executor.release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if update, ok := gate.TryUpdate(); ok {
			update.Release()
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("operation lease was not released after terminal persistence")
}

func TestFatalObservationErrorStopsClient(t *testing.T) {
	client := &Client{
		endpoint:     "wss://127.0.0.1:1/internal/agents/ws",
		identity:     identity.Identity{AgentID: "123e4567-e89b-42d3-a456-426614174000"},
		observations: failingObservationSource{},
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := client.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "IP observation monitor failed") {
		t.Fatalf("Run error = %v, want fatal monitor failure", err)
	}
}

func (*nilObservationSource) Ack(string) error { return nil }

func TestNewRejectsTypedNilObservationSource(t *testing.T) {
	var observations *nilObservationSource
	_, err := New(struct {
		Endpoint     string
		Identity     identity.Identity
		Version      string
		Capabilities []capability.Descriptor
		Executor     Executor
		Observations ObservationSource
		Lifecycle    *lifecycle.Gate
		OnReady      func() error
		Logger       *slog.Logger
	}{
		Endpoint: "wss://control.example/internal/agents/ws", Executor: &recordingExecutor{},
		Observations: observations, Lifecycle: lifecycle.New(),
	})
	if err == nil || !strings.Contains(err.Error(), "nil implementation") {
		t.Fatalf("New() error = %v, want typed nil rejection", err)
	}
}

func (e *recordingExecutor) Execute(_ context.Context, offer protocol.OperationOffer) protocol.ExecutionResult {
	e.executed <- offer.CommandID
	return protocol.ExecutionResult{Outcome: "failed", Code: "test", Result: map[string]any{}}
}

func TestAcceptedAckGatesExecution(t *testing.T) {
	executor := &recordingExecutor{executed: make(chan string, 2)}
	client := &Client{
		executor: executor, lifecycle: lifecycle.New(), running: map[string]*lifecycle.Lease{},
		pending:   map[string]pendingOperation{},
		completed: map[string]protocol.ExecutionResult{},
	}
	first := protocol.OperationOffer{
		CommandID: "123e4567-e89b-42d3-a456-426614174000",
		ExpiresAt: time.Now().Add(time.Minute),
	}
	firstLease, _ := client.lifecycle.TryOperation()
	client.pending[first.CommandID] = pendingOperation{offer: first, lease: firstLease}
	client.handleAcceptedAck(context.Background(), protocol.AcceptedAckBody{
		CommandID: first.CommandID, Accepted: false,
	})
	select {
	case id := <-executor.executed:
		t.Fatalf("rejected command executed: %s", id)
	case <-time.After(20 * time.Millisecond):
	}

	second := first
	second.CommandID = "123e4567-e89b-42d3-a456-426614174001"
	secondLease, _ := client.lifecycle.TryOperation()
	client.pending[second.CommandID] = pendingOperation{offer: second, lease: secondLease}
	client.handleAcceptedAck(context.Background(), protocol.AcceptedAckBody{
		CommandID: second.CommandID, Accepted: true,
	})
	select {
	case id := <-executor.executed:
		if id != second.CommandID {
			t.Fatalf("executed %s", id)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted command was not executed")
	}
}
