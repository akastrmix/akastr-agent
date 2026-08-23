package autoupdate

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const (
	TrialVersionEnvironment  = "AKASTR_AGENT_TRIAL_VERSION"
	TrialRevisionEnvironment = "AKASTR_AGENT_TRIAL_CONFIGURATION_REVISION"
	TrialReadinessTimeout    = 45 * time.Second
)

type Trial struct {
	version     string
	revision    int64
	releaseRoot string
	configRoot  string
	mu          sync.Mutex
	committed   bool
}

func LoadTrial(currentVersion string, currentRevision int64, releaseRoot, configPath string) (*Trial, error) {
	trialVersion := os.Getenv(TrialVersionEnvironment)
	if trialVersion == "" {
		return nil, nil
	}
	if trialVersion != currentVersion || !semanticVersion.MatchString(trialVersion) {
		return nil, errors.New("automatic update trial version is invalid")
	}
	trialRevision, err := strconv.ParseInt(os.Getenv(TrialRevisionEnvironment), 10, 64)
	if err != nil || trialRevision < 1 || trialRevision != currentRevision {
		return nil, errors.New("automatic maintenance trial revision is invalid")
	}
	if releaseRoot == "" || !filepath.IsAbs(releaseRoot) {
		return nil, errors.New("automatic update trial release root is invalid")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, errors.New("resolve automatic update trial executable")
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, err
	}
	expectedDeployment := filepath.Join(releaseRoot, "deployments", deploymentName(trialVersion, trialRevision))
	expected := filepath.Join(releaseRoot, "releases", trialVersion, "akastr-agent")
	expectedConfig := filepath.Join(expectedDeployment, "config", "config.json")
	if filepath.Clean(executable) != filepath.Clean(expected) || filepath.Clean(configPath) != filepath.Clean(expectedConfig) {
		return nil, errors.New("automatic maintenance trial is outside its approved deployment")
	}
	configDirectory, err := filepath.EvalSymlinks(filepath.Dir(configPath))
	if err != nil || filepath.Base(configDirectory) != strconv.FormatInt(trialRevision, 10) {
		return nil, errors.New("automatic maintenance trial configuration is outside its managed root")
	}
	return &Trial{
		version: trialVersion, revision: trialRevision, releaseRoot: releaseRoot,
		configRoot: filepath.Dir(configDirectory),
	}, nil
}

func (trial *Trial) Commit() (CommitResult, error) {
	trial.mu.Lock()
	defer trial.mu.Unlock()
	if trial.committed {
		return CommitResult{Committed: true}, nil
	}
	result, err := Commit(CommitOptions{
		Version: trial.version, ConfigurationRevision: trial.revision,
		ReleaseRoot: trial.releaseRoot, ConfigurationRoot: trial.configRoot,
	})
	if err != nil {
		return result, err
	}
	trial.committed = true
	if err := os.Unsetenv(TrialVersionEnvironment); err != nil {
		return result, errors.New("clear automatic update trial marker")
	}
	if err := os.Unsetenv(TrialRevisionEnvironment); err != nil {
		return result, errors.New("clear automatic maintenance trial revision marker")
	}
	return result, nil
}

func (trial *Trial) Discard() error {
	trial.mu.Lock()
	defer trial.mu.Unlock()
	if trial.committed {
		return nil
	}
	deploymentsRoot := filepath.Join(trial.releaseRoot, "deployments")
	target := filepath.Join(deploymentsRoot, deploymentName(trial.version, trial.revision))
	current, err := safeCurrentTarget(filepath.Join(trial.releaseRoot, "current"), deploymentsRoot)
	if err != nil {
		return err
	}
	if filepath.Clean(current) == filepath.Clean(target) {
		return nil
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("automatic update trial deployment is unsafe")
	}
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	return syncDirectory(deploymentsRoot)
}
