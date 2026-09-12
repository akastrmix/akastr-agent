package operation

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type blockingHandler struct {
	started, release chan struct{}
	runs, recoveries atomic.Int32
}

func (h *blockingHandler) Run(context.Context, protocol.OperationOffer) protocol.ExecutionResult {
	h.runs.Add(1)
	close(h.started)
	<-h.release
	return protocol.ExecutionResult{Outcome: "succeeded", Code: "completed", Result: map[string]any{"value": "durable"}}
}
func (h *blockingHandler) Recover(protocol.OperationOffer) protocol.ExecutionResult {
	h.recoveries.Add(1)
	return protocol.ExecutionResult{Outcome: "failed", Code: "interrupted_unknown"}
}

func TestExecutorJoinsLiveDuplicateAndReplaysAfterRestart(t *testing.T) {
	options := Options{StateFile: filepath.Join(t.TempDir(), "operations.json"), RecentLimit: 16}
	engine, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(engine)
	handler := &blockingHandler{started: make(chan struct{}), release: make(chan struct{})}
	offer := protocol.OperationOffer{CommandID: protocol.NewUUID(), CommandType: "changeip.execute"}
	done := make(chan error, 2)
	run := func() {
		result, err := executor.Execute(t.Context(), offer, "target-network", handler)
		if err == nil && result.Code != "completed" {
			t.Errorf("unexpected result: %+v", result)
		}
		done <- err
	}
	go run()
	<-handler.started
	duplicateContext, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := executor.Execute(duplicateContext, offer, "target-network", handler); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled duplicate must join, not recover, the live operation: %v", err)
	}
	if _, active := engine.Active(offer.CommandID); !active {
		t.Fatal("duplicate completed a still-running journal")
	}
	go run()
	close(handler.release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	restarted, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewExecutor(restarted).Execute(t.Context(), offer, "target-network", handler)
	if err != nil || result.Result["value"] != "durable" || handler.runs.Load() != 1 || handler.recoveries.Load() != 0 {
		t.Fatalf("result=%+v err=%v runs=%d recoveries=%d", result, err, handler.runs.Load(), handler.recoveries.Load())
	}
}
