package ws

import (
	"context"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type recordingExecutor struct {
	executed chan string
}

func (e *recordingExecutor) Execute(_ context.Context, offer protocol.OperationOffer) protocol.ExecutionResult {
	e.executed <- offer.CommandID
	return protocol.ExecutionResult{Outcome: "failed", Code: "test", Result: map[string]any{}}
}

func TestAcceptedAckGatesExecution(t *testing.T) {
	executor := &recordingExecutor{executed: make(chan string, 2)}
	client := &Client{
		executor: executor, running: map[string]struct{}{},
		pending:   map[string]protocol.OperationOffer{},
		completed: map[string]protocol.ExecutionResult{},
	}
	first := protocol.OperationOffer{
		CommandID: "123e4567-e89b-42d3-a456-426614174000",
		ExpiresAt: time.Now().Add(time.Minute),
	}
	client.pending[first.CommandID] = first
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
	client.pending[second.CommandID] = second
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
