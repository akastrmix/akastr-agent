package autoupdate

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxBinaryBytes = 32 * 1024 * 1024

var ErrOperationActive = errors.New("Agent operation became active before update switch")

type CommandRunner interface {
	Output(context.Context, string, ...string) (string, error)
}

type ApplyOptions struct {
	Manifest        Manifest
	ConfigPath      string
	ReleaseRoot     string
	HTTPClient      *http.Client
	Runner          CommandRunner
	OperationActive func() (bool, error)
}

func Apply(ctx context.Context, options ApplyOptions) error {
	if runtime.GOOS != "linux" {
		return errors.New("automatic updates are supported only on Linux")
	}
	if options.Manifest.Status != "update_available" {
		return errors.New("automatic update apply requires an available update")
	}
	if options.ConfigPath == "" || options.ReleaseRoot == "" || !filepath.IsAbs(options.ConfigPath) || !filepath.IsAbs(options.ReleaseRoot) {
		return errors.New("automatic update paths must be absolute")
	}
	releaseRoot, err := filepath.Abs(options.ReleaseRoot)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(releaseRoot); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Agent release root is not a safe directory")
	}
	releasesRoot := filepath.Join(releaseRoot, "releases")
	if info, err := os.Lstat(releasesRoot); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Agent releases root is not a safe directory")
	}
	runner := options.Runner
	if runner == nil {
		runner = systemRunner{}
	}
	staging, err := os.MkdirTemp(releasesRoot, ".update-")
	if err != nil {
		return fmt.Errorf("create Agent update staging directory: %w", err)
	}
	createdRelease := false
	defer func() { _ = os.RemoveAll(staging) }()
	if err := os.Chmod(staging, 0o700); err != nil {
		return err
	}
	stagedBinary := filepath.Join(staging, "akastr-agent")
	if err := downloadBinary(ctx, options.HTTPClient, options.Manifest, stagedBinary); err != nil {
		return err
	}
	if err := verifyBinary(ctx, runner, stagedBinary, options.Manifest.Version, options.ConfigPath); err != nil {
		return err
	}

	targetRelease := filepath.Join(releasesRoot, options.Manifest.Version)
	if info, err := os.Lstat(targetRelease); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("target Agent release path is unsafe")
		}
		existingBinary := filepath.Join(targetRelease, "akastr-agent")
		if err := verifyFileChecksum(existingBinary, options.Manifest.BinarySHA256); err != nil {
			return errors.New("existing target Agent release is not immutable")
		}
		if err := verifyBinary(ctx, runner, existingBinary, options.Manifest.Version, options.ConfigPath); err != nil {
			return errors.New("existing target Agent release failed validation")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		if err := os.Rename(staging, targetRelease); err != nil {
			return fmt.Errorf("install Agent update release: %w", err)
		}
		createdRelease = true
		staging = ""
	}

	currentLink := filepath.Join(releaseRoot, "current")
	previousTarget, err := safeCurrentTarget(currentLink, releasesRoot)
	if err != nil {
		if createdRelease {
			_ = os.RemoveAll(targetRelease)
		}
		return err
	}
	if options.OperationActive != nil {
		active, err := options.OperationActive()
		if err != nil || active {
			if createdRelease {
				_ = os.RemoveAll(targetRelease)
			}
			if err != nil {
				return fmt.Errorf("recheck active Agent operation: %w", err)
			}
			return ErrOperationActive
		}
	}
	if err := replaceSymlink(currentLink, targetRelease); err != nil {
		if createdRelease {
			_ = os.RemoveAll(targetRelease)
		}
		return err
	}

	pruneOldReleases(releasesRoot, targetRelease, previousTarget)
	return nil
}

func downloadBinary(ctx context.Context, httpClient *http.Client, manifest Manifest, destination string) error {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Minute}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, manifest.BinaryURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", "Akastr-Agent-Updater/"+manifest.Version)
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("download Agent update: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("download Agent update: server returned HTTP %d", response.StatusCode)
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	written, copyError := io.Copy(file, io.LimitReader(response.Body, maxBinaryBytes+1))
	syncError := file.Sync()
	closeError := file.Close()
	if copyError != nil || syncError != nil || closeError != nil {
		_ = os.Remove(destination)
		return errors.New("write Agent update binary failed")
	}
	if written == 0 || written > maxBinaryBytes {
		_ = os.Remove(destination)
		return errors.New("Agent update binary size is invalid")
	}
	if err := verifyFileChecksum(destination, manifest.BinarySHA256); err != nil {
		_ = os.Remove(destination)
		return err
	}
	return nil
}

func verifyFileChecksum(path, expected string) error {
	expectedBytes, err := DecodeChecksum(expected)
	if err != nil {
		return errors.New("Agent update checksum is invalid")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o100 == 0 {
		return errors.New("Agent update binary is not a non-writable executable regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if !bytesEqual(hash.Sum(nil), expectedBytes) {
		return errors.New("Agent update binary checksum mismatch")
	}
	return nil
}

func verifyBinary(ctx context.Context, runner CommandRunner, binary, version, configPath string) error {
	actualVersion, err := runner.Output(ctx, binary, "version")
	if err != nil || strings.TrimSpace(actualVersion) != version {
		return errors.New("Agent update binary version mismatch")
	}
	if _, err := runner.Output(ctx, binary, "check-config", "--config", configPath); err != nil {
		return errors.New("Agent update binary rejected the current configuration")
	}
	return nil
}

func safeCurrentTarget(currentLink, releasesRoot string) (string, error) {
	info, err := os.Lstat(currentLink)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", errors.New("Agent current release is not a symlink")
	}
	target, err := filepath.EvalSymlinks(currentLink)
	if err != nil {
		return "", errors.New("Agent current release target is invalid")
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(releasesRoot, target)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("Agent current release escapes the releases root")
	}
	info, err = os.Lstat(target)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Agent current release target is unsafe")
	}
	return target, nil
}

func replaceSymlink(path, target string) error {
	temporary := fmt.Sprintf("%s.update-%d", path, os.Getpid())
	_ = os.Remove(temporary)
	if err := os.Symlink(target, temporary); err != nil {
		return fmt.Errorf("create Agent current symlink: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace Agent current symlink: %w", err)
	}
	return nil
}

func pruneOldReleases(releasesRoot, current, previous string) {
	entries, err := os.ReadDir(releasesRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(releasesRoot, entry.Name())
		if path == current || path == previous || !semanticVersion.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		_ = os.RemoveAll(path)
	}
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var different byte
	for index := range left {
		different |= left[index] ^ right[index]
	}
	return different == 0
}

type systemRunner struct{}

func (systemRunner) Output(ctx context.Context, name string, arguments ...string) (string, error) {
	output, err := exec.CommandContext(ctx, name, arguments...).Output()
	return string(output), err
}
