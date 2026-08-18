package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

type JSONFile struct {
	path          string
	mu            sync.Mutex
	syncDirectory func(string) error
}

func NewJSONFile(path string) *JSONFile {
	return &JSONFile{path: path, syncDirectory: syncDirectory}
}

func (f *JSONFile) Load(destination any) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	file, err := os.Open(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open state file: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return false, fmt.Errorf("decode state file: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return false, errors.New("decode state file: multiple JSON values")
		}
		return false, fmt.Errorf("decode state file trailing data: %w", err)
	}
	return true, nil
}

func (f *JSONFile) Save(value any) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	directory := filepath.Dir(f.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	temporary, err := os.CreateTemp(directory, ".akastr-agent-state-*")
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}
	temporaryPath := temporary.Name()
	keepTemporary := true
	defer func() {
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set temporary state permissions: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode state file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary state file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary state file: %w", err)
	}
	if err := os.Rename(temporaryPath, f.path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	keepTemporary = false

	return f.syncDirectory(directory)
}

func (f *JSONFile) Remove() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := os.Remove(f.path); err != nil {
		return fmt.Errorf("remove state file: %w", err)
	}
	return f.syncDirectory(filepath.Dir(f.path))
}

func syncDirectory(directory string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open state directory for sync: %w", err)
	}
	if err := directoryHandle.Sync(); err != nil {
		_ = directoryHandle.Close()
		return fmt.Errorf("sync state directory: %w", err)
	}
	if err := directoryHandle.Close(); err != nil {
		return fmt.Errorf("close state directory: %w", err)
	}
	return nil
}
