package ipqualityrunner

import (
	"path/filepath"
	"testing"

	"github.com/akastrmix/akastr-agent/internal/operation"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

func TestActiveJournalRecoveryReleasesRunnerWithoutExecuting(t *testing.T) {
	engine, err := operation.Open(operation.Options{
		StateFile: filepath.Join(t.TempDir(), "operations.json"), RecentLimit: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	commandID := "123e4567-e89b-42d3-a456-426614174001"
	if _, err := engine.Begin(commandID, "ipquality.execute", "ipquality-runner"); err != nil {
		t.Fatal(err)
	}
	handler := New(engine, nil, "2026.08.13")
	result := handler.Execute(t.Context(), protocol.OperationOffer{CommandID: commandID})
	if result.Outcome != "failed" || result.Code != "interrupted_unknown" {
		t.Fatalf("Execute() = %#v", result)
	}
	if _, active := engine.Active(commandID); active {
		t.Fatal("active runner journal was not released")
	}
	if recent, found := engine.Recent(commandID); !found || len(recent.TerminalResult) == 0 {
		t.Fatal("recovery result was not persisted for replay")
	}
}
