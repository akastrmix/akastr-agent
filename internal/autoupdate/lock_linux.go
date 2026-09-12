package autoupdate

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// The installer uses flock on this same file. Close-on-exec releases it before
// the candidate starts; trial commit and discard acquire their own lock.
func lockMaintenance(root string) (*os.File, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(root) || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("unsafe maintenance root")
	}
	fd, err := syscall.Open(filepath.Join(root, ".maintenance.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "maintenance lock")
	info, err = file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("unsafe maintenance lock")
	}
	if err == nil {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
