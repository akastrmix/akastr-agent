package autoupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const maxBinaryBytes = 32 * 1024 * 1024

type CommandRunner interface {
	Output(context.Context, string, ...string) (string, error)
}

type ApplyOptions struct {
	Manifest      Manifest
	ConfigPath    string
	ReleaseRoot   string
	HTTPClient    *http.Client
	Runner        CommandRunner
	SyncDirectory func(string) error
}

type StagedRelease struct {
	Version string
	Binary  string
}

type CommitOptions struct {
	Version               string
	ConfigurationRevision int64
	ReleaseRoot           string
	ConfigurationRoot     string
	SyncDirectory         func(string) error
	RemoveAll             func(string) error
}

type CommitResult struct {
	Committed     bool
	CleanupFailed bool
}

func Stage(ctx context.Context, options ApplyOptions) (StagedRelease, error) {
	if runtime.GOOS != "linux" {
		return StagedRelease{}, errors.New("automatic updates are supported only on Linux")
	}
	if options.Manifest.Software.Status != "update_available" {
		return StagedRelease{}, errors.New("automatic update stage requires an available update")
	}
	if options.ConfigPath == "" || options.ReleaseRoot == "" || !filepath.IsAbs(options.ConfigPath) || !filepath.IsAbs(options.ReleaseRoot) {
		return StagedRelease{}, errors.New("automatic update paths must be absolute")
	}
	releaseRoot, err := filepath.Abs(options.ReleaseRoot)
	if err != nil {
		return StagedRelease{}, err
	}
	if info, err := os.Lstat(releaseRoot); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return StagedRelease{}, errors.New("Agent release root is not a safe directory")
	}
	releasesRoot := filepath.Join(releaseRoot, "releases")
	if info, err := os.Lstat(releasesRoot); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return StagedRelease{}, errors.New("Agent releases root is not a safe directory")
	}
	if _, err := safeCurrentTarget(filepath.Join(releaseRoot, "current"), filepath.Join(releaseRoot, "deployments")); err != nil {
		return StagedRelease{}, err
	}
	runner := options.Runner
	if runner == nil {
		runner = systemRunner{}
	}
	targetRelease := filepath.Join(releasesRoot, options.Manifest.Software.Version)
	if info, err := os.Lstat(targetRelease); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return StagedRelease{}, errors.New("target Agent release path is unsafe")
		}
		existingBinary := filepath.Join(targetRelease, "akastr-agent")
		if err := verifyFileChecksum(existingBinary, options.Manifest.Software.BinarySHA256); err != nil {
			var pathError *os.PathError
			if errors.As(err, &pathError) {
				return StagedRelease{}, err
			}
			return StagedRelease{}, fmt.Errorf("%w: existing target Agent release is not immutable", ErrCandidateRejected)
		}
		if err := verifyBinary(ctx, runner, existingBinary, options.Manifest.Software.Version); err != nil {
			return StagedRelease{}, fmt.Errorf("existing target Agent release failed validation: %w", err)
		}
		if options.Manifest.Configuration.Status == "current" {
			if err := verifyBinaryConfiguration(ctx, runner, existingBinary, options.ConfigPath); err != nil {
				return StagedRelease{}, fmt.Errorf("existing target Agent release rejected the current configuration: %w", err)
			}
		}
		return StagedRelease{Version: options.Manifest.Software.Version, Binary: existingBinary}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return StagedRelease{}, err
	}
	staging, err := os.MkdirTemp(releasesRoot, ".update-")
	if err != nil {
		return StagedRelease{}, fmt.Errorf("create Agent update staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := os.Chmod(staging, 0o700); err != nil {
		return StagedRelease{}, err
	}
	stagedBinary := filepath.Join(staging, "akastr-agent")
	if err := downloadBinary(ctx, options.HTTPClient, options.Manifest, stagedBinary); err != nil {
		return StagedRelease{}, err
	}
	if err := verifyBinary(ctx, runner, stagedBinary, options.Manifest.Software.Version); err != nil {
		return StagedRelease{}, err
	}

	syncFn := options.SyncDirectory
	if syncFn == nil {
		syncFn = syncDirectory
	}
	if err := syncFn(staging); err != nil {
		return StagedRelease{}, fmt.Errorf("sync Agent update staging directory: %w", err)
	}
	if err := os.Rename(staging, targetRelease); err != nil {
		return StagedRelease{}, fmt.Errorf("install Agent update release: %w", err)
	}
	staging = ""
	if err := syncFn(releasesRoot); err != nil {
		return StagedRelease{}, fmt.Errorf("sync Agent releases directory: %w", err)
	}
	if options.Manifest.Configuration.Status == "current" {
		if err := verifyBinaryConfiguration(ctx, runner, filepath.Join(targetRelease, "akastr-agent"), options.ConfigPath); err != nil {
			return StagedRelease{}, err
		}
	}
	return StagedRelease{Version: options.Manifest.Software.Version, Binary: filepath.Join(targetRelease, "akastr-agent")}, nil
}

func Commit(options CommitOptions) (CommitResult, error) {
	lock, err := lockMaintenance(options.ReleaseRoot)
	if err != nil {
		return CommitResult{}, err
	}
	defer lock.Close()
	if runtime.GOOS != "linux" {
		return CommitResult{}, errors.New("automatic updates are supported only on Linux")
	}
	if !semanticVersion.MatchString(options.Version) || options.ConfigurationRevision < 1 ||
		options.ReleaseRoot == "" || !filepath.IsAbs(options.ReleaseRoot) ||
		options.ConfigurationRoot == "" || !filepath.IsAbs(options.ConfigurationRoot) {
		return CommitResult{}, errors.New("automatic update commit options are invalid")
	}
	releaseRoot, err := filepath.Abs(options.ReleaseRoot)
	if err != nil {
		return CommitResult{}, err
	}
	configurationRoot, err := filepath.Abs(options.ConfigurationRoot)
	if err != nil {
		return CommitResult{}, err
	}
	deploymentsRoot := filepath.Join(releaseRoot, "deployments")
	currentLink := filepath.Join(releaseRoot, "current")
	previous, err := safeCurrentTarget(currentLink, deploymentsRoot)
	if err != nil {
		return CommitResult{}, err
	}
	target := filepath.Join(deploymentsRoot, deploymentName(options.Version, options.ConfigurationRevision))
	if info, statError := os.Lstat(target); statError != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return CommitResult{}, errors.New("staged Agent release is not a safe directory")
	}
	if previous == target {
		return CommitResult{Committed: true}, nil
	}
	syncFn := options.SyncDirectory
	if syncFn == nil {
		syncFn = syncDirectory
	}
	// Store the predecessor in the candidate before switching current. The single
	// current rename then publishes both pointers atomically, including after a crash.
	if _, err := replaceSymlink(filepath.Join(target, "previous"), previous, syncFn); err != nil {
		return CommitResult{}, err
	}
	committed, err := replaceSymlink(currentLink, target, syncFn)
	result := CommitResult{Committed: committed}
	if err != nil {
		return result, err
	}
	removeAll := options.RemoveAll
	if removeAll == nil {
		removeAll = os.RemoveAll
	}
	result.CleanupFailed = pruneManagedArtifacts(releaseRoot, configurationRoot, target, previous, removeAll)
	return result, nil
}

func StageDeployment(releaseRoot, version string, revision int64, configPath string) (string, error) {
	if !semanticVersion.MatchString(version) || revision < 1 || !filepath.IsAbs(releaseRoot) || !filepath.IsAbs(configPath) {
		return "", errors.New("Agent deployment options are invalid")
	}
	releaseDirectory := filepath.Join(releaseRoot, "releases", version)
	binary := filepath.Join(releaseDirectory, "akastr-agent")
	if info, err := os.Lstat(binary); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Agent deployment binary is unsafe")
	}
	if info, err := os.Lstat(configPath); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Agent deployment configuration is unsafe")
	}
	configDirectory, err := filepath.EvalSymlinks(filepath.Dir(configPath))
	if err != nil {
		return "", errors.New("Agent deployment configuration directory is invalid")
	}
	configDirectory, err = filepath.Abs(configDirectory)
	if err != nil {
		return "", err
	}
	deploymentsRoot := filepath.Join(releaseRoot, "deployments")
	if err := os.MkdirAll(deploymentsRoot, 0o755); err != nil {
		return "", err
	}
	if info, err := os.Lstat(deploymentsRoot); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Agent deployments root is unsafe")
	}
	target := filepath.Join(deploymentsRoot, deploymentName(version, revision))
	if info, err := os.Lstat(target); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("Agent deployment target is unsafe")
		}
		if err := validateDeployment(target, binary, configDirectory); err != nil {
			return "", err
		}
		return target, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	staging, err := os.MkdirTemp(deploymentsRoot, ".deployment-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if err := os.Symlink(binary, filepath.Join(staging, "akastr-agent")); err != nil {
		return "", err
	}
	if err := os.Symlink(configDirectory, filepath.Join(staging, "config")); err != nil {
		return "", err
	}
	if err := syncDirectory(staging); err != nil {
		return "", err
	}
	if err := os.Rename(staging, target); err != nil {
		return "", err
	}
	if err := syncDirectory(deploymentsRoot); err != nil {
		return "", err
	}
	if err := validateDeployment(target, binary, configDirectory); err != nil {
		return "", err
	}
	return target, nil
}

func validateDeployment(directory, binary, configDirectory string) error {
	for name, expected := range map[string]string{"akastr-agent": binary, "config": configDirectory} {
		link := filepath.Join(directory, name)
		info, err := os.Lstat(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return errors.New("Agent deployment link is unsafe")
		}
		resolved, err := filepath.EvalSymlinks(link)
		if err != nil {
			return errors.New("Agent deployment link target is invalid")
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil || filepath.Clean(resolved) != filepath.Clean(expected) {
			return errors.New("Agent deployment link target is mismatched")
		}
	}
	return nil
}

func deploymentName(version string, revision int64) string {
	return version + "-r" + strconv.FormatInt(revision, 10)
}

func downloadBinary(ctx context.Context, httpClient *http.Client, manifest Manifest, destination string) error {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Minute}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, manifest.Software.BinaryURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", "Akastr-Agent-Updater/"+manifest.Software.Version)
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
	if err := verifyFileChecksum(destination, manifest.Software.BinarySHA256); err != nil {
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
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o100 == 0 {
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
	if !bytes.Equal(hash.Sum(nil), expectedBytes) {
		return errors.New("Agent update binary checksum mismatch")
	}
	return nil
}

func verifyBinary(ctx context.Context, runner CommandRunner, binary, version string) error {
	actualVersion, err := runner.Output(ctx, binary, "version")
	if err != nil {
		return candidateCommandError(ctx, err)
	}
	if strings.TrimSpace(actualVersion) != version {
		return fmt.Errorf("%w: Agent update binary version mismatch", ErrCandidateRejected)
	}
	return nil
}

func verifyBinaryConfiguration(ctx context.Context, runner CommandRunner, binary, configPath string) error {
	if _, err := runner.Output(ctx, binary, "check-config", "--config", configPath); err != nil {
		return fmt.Errorf("Agent update binary rejected the current configuration: %w", candidateCommandError(ctx, err))
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

func replaceSymlink(path, target string, syncFn func(string) error) (bool, error) {
	temporary := fmt.Sprintf("%s.update-%d", path, os.Getpid())
	_ = os.Remove(temporary)
	if err := os.Symlink(target, temporary); err != nil {
		return false, fmt.Errorf("create Agent current symlink: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return false, fmt.Errorf("replace Agent current symlink: %w", err)
	}
	if err := syncFn(filepath.Dir(path)); err != nil {
		return true, fmt.Errorf("sync Agent current directory: %w", err)
	}
	return true, nil
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
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

func pruneOldDeployments(deploymentsRoot, current, previous string, removeAll func(string) error) bool {
	return pruneManagedDirectories(
		deploymentsRoot,
		deploymentPattern,
		map[string]struct{}{filepath.Clean(current): {}, filepath.Clean(previous): {}},
		removeAll,
	)
}

func pruneManagedArtifacts(releaseRoot, configurationRoot, current, previous string, removeAll func(string) error) bool {
	releasesRoot := filepath.Join(releaseRoot, "releases")
	deploymentsRoot := filepath.Join(releaseRoot, "deployments")
	for _, root := range []string{releasesRoot, deploymentsRoot, configurationRoot} {
		info, err := os.Lstat(root)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	protectedReleases := make(map[string]struct{}, 2)
	protectedConfigurations := make(map[string]struct{}, 2)
	for _, deployment := range []string{current, previous} {
		release, configuration, err := managedDeploymentArtifacts(deployment, releasesRoot, configurationRoot)
		if err != nil {
			return true
		}
		protectedReleases[release] = struct{}{}
		protectedConfigurations[configuration] = struct{}{}
	}
	if pruneOldDeployments(deploymentsRoot, current, previous, removeAll) {
		return true
	}
	failed := pruneManagedDirectories(releasesRoot, semanticVersion, protectedReleases, removeAll)
	if pruneManagedDirectories(configurationRoot, configurationRevisionPattern, protectedConfigurations, removeAll) {
		failed = true
	}
	return failed
}

func managedDeploymentArtifacts(deployment, releasesRoot, configurationRoot string) (string, string, error) {
	deployment = filepath.Clean(deployment)
	name := filepath.Base(deployment)
	if !deploymentPattern.MatchString(name) {
		return "", "", errors.New("protected Agent deployment name is invalid")
	}
	separator := strings.LastIndex(name, "-r")
	version, revision := name[:separator], name[separator+2:]

	binaryLink := filepath.Join(deployment, "akastr-agent")
	info, err := os.Lstat(binaryLink)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", "", errors.New("protected Agent deployment binary link is invalid")
	}
	binary, err := filepath.EvalSymlinks(binaryLink)
	if err != nil {
		return "", "", errors.New("protected Agent deployment binary target is invalid")
	}
	release, err := directManagedDirectory(filepath.Dir(binary), releasesRoot, semanticVersion)
	if err != nil || filepath.Base(release) != version || filepath.Base(binary) != "akastr-agent" {
		return "", "", errors.New("protected Agent deployment release is invalid")
	}

	configLink := filepath.Join(deployment, "config")
	info, err = os.Lstat(configLink)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", "", errors.New("protected Agent deployment configuration link is invalid")
	}
	configuration, err := filepath.EvalSymlinks(configLink)
	if err != nil {
		return "", "", errors.New("protected Agent deployment configuration target is invalid")
	}
	configuration, err = directManagedDirectory(configuration, configurationRoot, configurationRevisionPattern)
	if err != nil || filepath.Base(configuration) != revision {
		return "", "", errors.New("protected Agent deployment configuration is invalid")
	}
	return release, configuration, nil
}

func directManagedDirectory(path, root string, pattern *regexp.Regexp) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || filepath.Dir(relative) != "." || !pattern.MatchString(filepath.Base(path)) {
		return "", errors.New("managed Agent artifact escapes its root")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("managed Agent artifact is not a safe directory")
	}
	return filepath.Clean(path), nil
}

func pruneManagedDirectories(root string, pattern *regexp.Regexp, protected map[string]struct{}, removeAll func(string) error) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return true
	}
	failed := false
	for _, entry := range entries {
		path := filepath.Clean(filepath.Join(root, entry.Name()))
		if _, keep := protected[path]; keep || !pattern.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := removeAll(path); err != nil {
			failed = true
		}
	}
	return failed
}

type systemRunner struct{}

func (systemRunner) Output(ctx context.Context, name string, arguments ...string) (string, error) {
	output, err := exec.CommandContext(ctx, name, arguments...).Output()
	return string(output), err
}
