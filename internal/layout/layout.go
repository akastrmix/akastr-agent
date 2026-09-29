// Package layout owns the fixed on-disk arrangement shared by the installer,
// the updater and the running Agent.
//
//	/usr/local/lib/akastr-agent/
//	  current -> slots/a          active slot, switched by one atomic rename
//	  slots/{a,b}/akastr-agent    binary of that slot
//	  slots/{a,b}/config.json     Cloud configuration of that slot (root-only)
//	  ipquality/<sha256>.sh       pinned Runner script, one file per pin
//	  .maintenance.lock           flock guarding this directory; every Agent
//	                              version must use this exact path
//	/etc/akastr-agent/identity.json
//	/var/lib/akastr-agent/
//	  {state,ip-state}.json       execution journal and IP facts
//	  update-attempt.json         bounded candidate attempts for one target
package layout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	BinaryName = "akastr-agent"
	ConfigName = "config.json"
)

var slotNames = [2]string{"a", "b"}

type Layout struct {
	Root         string
	IdentityFile string
	StateDir     string
}

func Default() Layout {
	return Layout{
		Root:         "/usr/local/lib/akastr-agent",
		IdentityFile: "/etc/akastr-agent/identity.json",
		StateDir:     "/var/lib/akastr-agent",
	}
}

func (l Layout) StateFile() string   { return filepath.Join(l.StateDir, "state.json") }
func (l Layout) IPStateFile() string { return filepath.Join(l.StateDir, "ip-state.json") }
func (l Layout) AttemptFile() string { return filepath.Join(l.StateDir, "update-attempt.json") }
func (l Layout) Current() string     { return filepath.Join(l.Root, "current") }

func (l Layout) Slot(name string) string { return filepath.Join(l.Root, "slots", name) }

func (l Layout) IPQualityScript(sha256Hex string) string {
	return filepath.Join(l.Root, "ipquality", sha256Hex+".sh")
}

func SlotBinary(slot string) string { return filepath.Join(slot, BinaryName) }
func SlotConfig(slot string) string { return filepath.Join(slot, ConfigName) }

// ActiveSlot returns the slot that current points to, or "" when current is
// missing or points outside the two slots.
func (l Layout) ActiveSlot() (string, error) {
	target, err := os.Readlink(l.Current())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read current Agent slot: %w", err)
	}
	for _, name := range slotNames {
		if target == filepath.Join("slots", name) {
			return l.Slot(name), nil
		}
	}
	return "", nil
}

// InactiveSlot returns the slot that may be overwritten without touching the
// running deployment.
func (l Layout) InactiveSlot() (string, error) {
	active, err := l.ActiveSlot()
	if err != nil {
		return "", err
	}
	if active == l.Slot("a") {
		return l.Slot("b"), nil
	}
	return l.Slot("a"), nil
}

// Candidate reports whether configPath belongs to a slot that is not active,
// which is how an updater-started process recognises itself.
func (l Layout) Candidate(configPath string) (string, bool, error) {
	absolute, err := filepath.Abs(configPath)
	if err != nil {
		return "", false, err
	}
	slot := filepath.Dir(absolute)
	if slot != l.Slot("a") && slot != l.Slot("b") {
		return "", false, nil
	}
	active, err := l.ActiveSlot()
	if err != nil {
		return "", false, err
	}
	return slot, slot != active, nil
}

// PrepareSlot empties a slot so that a new binary and configuration can be
// written into it.
func (l Layout) PrepareSlot(slot string) error {
	if slot != l.Slot("a") && slot != l.Slot("b") {
		return errors.New("Agent slot is outside the managed layout")
	}
	if err := os.RemoveAll(slot); err != nil {
		return err
	}
	return os.MkdirAll(slot, 0o700)
}

// Activate points current at slot with a single atomic rename.
func (l Layout) Activate(slot string) error {
	relative, err := filepath.Rel(l.Root, slot)
	if err != nil || (relative != filepath.Join("slots", "a") && relative != filepath.Join("slots", "b")) {
		return errors.New("Agent slot is outside the managed layout")
	}
	temporary := l.Current() + ".new"
	_ = os.Remove(temporary)
	if err := os.Symlink(relative, temporary); err != nil {
		return fmt.Errorf("create current Agent link: %w", err)
	}
	if err := os.Rename(temporary, l.Current()); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("switch current Agent slot: %w", err)
	}
	return SyncDirectory(l.Root)
}

// Lock takes the maintenance lock. The descriptor is close-on-exec, so an
// updater's lock is released when it replaces itself with a candidate.
func (l Layout) Lock() (*os.File, error) {
	if err := os.MkdirAll(l.Root, 0o755); err != nil {
		return nil, err
	}
	// Old and new versions overlap during updates and reinstalls, so moving
	// this path would let each take its own lock and clear the other's slot.
	fd, err := syscall.Open(filepath.Join(l.Root, ".maintenance.lock"),
		syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = syscall.Close(fd)
		return nil, ErrLocked
	}
	return os.NewFile(uintptr(fd), "maintenance lock"), nil
}

var ErrLocked = errors.New("Agent installation or update is already running")

func SyncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

// WriteFile writes a complete file next to its destination and renames it into
// place, so a crash never leaves a partially written binary or configuration.
func WriteFile(path string, contents []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
