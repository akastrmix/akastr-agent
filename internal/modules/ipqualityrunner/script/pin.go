package script

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The official script is pinned by commit and digest. Each pin is stored under
// its own digest, so a candidate that changes the pin never alters the script
// used by the deployment it may roll back to.
const (
	PinnedCommit  = "0ee5f192fed70c04615852efba0e4b8bd43546c7"
	PinnedVersion = "0ee5f192fed7"
	PinnedSHA256  = "9823c560e0d19769eb627329a31cb47da655d087166d86e40d9b6c77bc7f32fb"
)

// Debian packages that provide requiredCommands. Keep each list on one line:
// scripts/verify-debian-runtime-dependencies.sh reads them.
const (
	RunnerPackages = "bash bc dnsutils iproute2 jq netcat-openbsd"
	RunnerCommands = "/bin/bash bc curl dig ip jq nc"
)

// EnsurePinnedScript downloads and verifies the pinned script when path does
// not already hold it.
func EnsurePinnedScript(ctx context.Context, client *http.Client, path string) error {
	if contents, err := os.ReadFile(path); err == nil && digest(contents) == PinnedSHA256 {
		return nil
	}
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://raw.githubusercontent.com/xykt/IPQuality/"+PinnedCommit+"/ip.sh", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download IPQuality script: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download IPQuality script: HTTP %d", response.StatusCode)
	}
	var contents bytes.Buffer
	if _, err := io.Copy(&contents, io.LimitReader(response.Body, 4*1024*1024)); err != nil {
		return fmt.Errorf("download IPQuality script: %w", err)
	}
	if digest(contents.Bytes()) != PinnedSHA256 {
		return errors.New("IPQuality script integrity check failed")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := path + ".download"
	if err := os.WriteFile(temporary, contents.Bytes(), 0o755); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// RemoveOtherScripts deletes scripts of earlier pins from directory, which
// holds one script per pin. Only a committed deployment calls it: a later
// candidate downloads its own pin again.
func RemoveOtherScripts(directory string) error {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == PinnedSHA256+".sh" {
			continue
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func digest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}
