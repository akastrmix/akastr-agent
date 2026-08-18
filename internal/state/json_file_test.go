package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testState struct {
	SchemaVersion int    `json:"schema_version"`
	Value         string `json:"value"`
}

func TestJSONFileRoundTripAndReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	file := NewJSONFile(path)
	for _, value := range []string{"first", "second"} {
		if err := file.Save(testState{SchemaVersion: 1, Value: value}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	var loaded testState
	found, err := file.Load(&loaded)
	if err != nil || !found {
		t.Fatalf("Load() = %v, %v", found, err)
	}
	if loaded.Value != "second" {
		t.Fatalf("Load() value = %q", loaded.Value)
	}
}

func TestJSONFileRejectsUnknownOrCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"value":"ok","unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var loaded testState
	_, err := NewJSONFile(path).Load(&loaded)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Load() error = %v, want unknown field", err)
	}
}

func TestJSONFileMissingIsNotAnError(t *testing.T) {
	var loaded testState
	found, err := NewJSONFile(filepath.Join(t.TempDir(), "missing.json")).Load(&loaded)
	if err != nil || found {
		t.Fatalf("Load() = %v, %v", found, err)
	}
}

func TestJSONFilePropagatesDirectorySyncFailure(t *testing.T) {
	want := errors.New("directory fsync failed")
	path := filepath.Join(t.TempDir(), "state.json")
	file := NewJSONFile(path)
	file.syncDirectory = func(string) error { return want }
	if err := file.Save(testState{SchemaVersion: 1, Value: "durable"}); !errors.Is(err, want) {
		t.Fatalf("Save() error = %v, want %v", err, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("renamed state file missing after directory sync failure: %v", err)
	}
	if err := file.Remove(); !errors.Is(err, want) {
		t.Fatalf("Remove() error = %v, want %v", err, want)
	}
}
