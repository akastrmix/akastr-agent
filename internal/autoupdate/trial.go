package autoupdate

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	TrialVersionEnvironment = "AKASTR_AGENT_TRIAL_VERSION"
	TrialReadinessTimeout   = 45 * time.Second
)

type Trial struct {
	version     string
	releaseRoot string
	mu          sync.Mutex
	committed   bool
}

func LoadTrial(currentVersion, releaseRoot string) (*Trial, error) {
	trialVersion := os.Getenv(TrialVersionEnvironment)
	if trialVersion == "" {
		return nil, nil
	}
	if trialVersion != currentVersion || !semanticVersion.MatchString(trialVersion) {
		return nil, errors.New("automatic update trial version is invalid")
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
	expected := filepath.Join(releaseRoot, "releases", trialVersion, "akastr-agent")
	if filepath.Clean(executable) != filepath.Clean(expected) {
		return nil, errors.New("automatic update trial executable is outside its approved release")
	}
	return &Trial{version: trialVersion, releaseRoot: releaseRoot}, nil
}

func (trial *Trial) Commit() (CommitResult, error) {
	trial.mu.Lock()
	defer trial.mu.Unlock()
	if trial.committed {
		return CommitResult{Committed: true}, nil
	}
	result, err := Commit(CommitOptions{Version: trial.version, ReleaseRoot: trial.releaseRoot})
	if err != nil {
		return result, err
	}
	trial.committed = true
	if err := os.Unsetenv(TrialVersionEnvironment); err != nil {
		return result, errors.New("clear automatic update trial marker")
	}
	return result, nil
}
