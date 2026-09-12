package autoupdate

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

var stagingDirectoryPattern = regexp.MustCompile(`^(\.update-[0-9]+|\.deployment-[0-9]+|\.configuration[-.][0-9]+|\.install-[A-Za-z0-9]+)$`)
var stagingFilePattern = regexp.MustCompile(`^(\.akastr-agent\.[0-9]+|\.akastr-agent-state-[0-9]+)$`)
var stagingLinkPattern = regexp.MustCompile(`^((current|previous)\.update-[0-9]+|\.(current|previous)\.[0-9]+)$`)

// Called only under the shared maintenance lock. No process can still be writing
// these staging directories; durable identity, operation and retry files are excluded.
func cleanupStaging(root, configRoot string) error {
	var failures []error
	directories := []string{root, filepath.Join(root, "releases"), filepath.Join(root, "deployments"), configRoot}
	for _, group := range []struct {
		path    string
		pattern *regexp.Regexp
	}{{filepath.Join(root, "releases"), semanticVersion}, {filepath.Join(root, "deployments"), deploymentPattern}} {
		info, err := os.Lstat(group.path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		entries, err := os.ReadDir(group.path)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() && group.pattern.MatchString(entry.Name()) {
				directories = append(directories, filepath.Join(group.path, entry.Name()))
			}
		}
	}
	for _, directory := range directories {
		info, err := os.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			failures = append(failures, errors.New("unsafe staging root"))
			continue
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, entry := range entries {
			path := filepath.Join(directory, entry.Name())
			if entry.IsDir() && stagingDirectoryPattern.MatchString(entry.Name()) {
				if err := os.RemoveAll(path); err != nil {
					failures = append(failures, err)
				}
			} else if entry.Type().IsRegular() && stagingFilePattern.MatchString(entry.Name()) {
				if err := os.Remove(path); err != nil {
					failures = append(failures, err)
				}
			} else if entry.Type()&os.ModeSymlink != 0 && stagingLinkPattern.MatchString(entry.Name()) {
				if err := os.Remove(path); err != nil {
					failures = append(failures, err)
				}
			}
		}
	}
	return errors.Join(failures...)
}

// Keep current, the explicitly recorded previous deployment, and the one desired
// candidate. An older installation without a previous pointer is never guessed.
func cleanupCandidates(root, configRoot, version string, revision int64) error {
	deployments := filepath.Join(root, "deployments")
	current, err := safeCurrentTarget(filepath.Join(root, "current"), deployments)
	if err != nil {
		return err
	}
	previousLink := filepath.Join(current, "previous")
	if _, err := os.Lstat(previousLink); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	previous, err := safeCurrentTarget(previousLink, deployments)
	if err != nil {
		return err
	}
	protectedDeployments := map[string]struct{}{current: {}, previous: {}}
	protectedReleases := map[string]struct{}{}
	protectedConfigurations := map[string]struct{}{}
	for _, deployment := range []string{current, previous} {
		release, configuration, err := managedDeploymentArtifacts(deployment, filepath.Join(root, "releases"), configRoot)
		if err != nil {
			return err
		}
		protectedReleases[release] = struct{}{}
		protectedConfigurations[configuration] = struct{}{}
	}
	if version != "" {
		if !semanticVersion.MatchString(version) || revision < 1 {
			return errors.New("invalid cleanup candidate")
		}
		protectedDeployments[filepath.Join(deployments, deploymentName(version, revision))] = struct{}{}
		protectedReleases[filepath.Join(root, "releases", version)] = struct{}{}
		protectedConfigurations[filepath.Join(configRoot, strconv.FormatInt(revision, 10))] = struct{}{}
	}
	if pruneManagedDirectories(deployments, deploymentPattern, protectedDeployments, os.RemoveAll) {
		return errors.New("deployment cleanup incomplete")
	}
	failed := pruneManagedDirectories(filepath.Join(root, "releases"), semanticVersion, protectedReleases, os.RemoveAll)
	if pruneManagedDirectories(configRoot, configurationRevisionPattern, protectedConfigurations, os.RemoveAll) {
		failed = true
	}
	if failed {
		return errors.New("artifact cleanup incomplete")
	}
	return nil
}
