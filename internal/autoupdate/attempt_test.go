package autoupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAttemptsSurviveRestartAndManualGrantIsSingleUse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed deployments require Unix symlinks")
	}
	root, _, _ := releaseFixture(t)
	load := func(id string) (*attemptLedger, bool) {
		t.Helper()
		ledger, manual, err := loadAttempts(root, "v0.7.1", 1, id)
		if err != nil {
			t.Fatal(err)
		}
		return ledger, manual
	}
	first, _ := load("")
	if err := first.begin(); err != nil {
		t.Fatal(err)
	}
	second, _ := load("")
	if second.record.Attempts != 1 {
		t.Fatal("restart forgot first attempt")
	}
	if err := second.begin(); err != nil {
		t.Fatal(err)
	}
	exhausted, _ := load("")
	if err := exhausted.begin(); err == nil {
		t.Fatal("third automatic trial accepted")
	}
	manual, granted := load("first-click")
	if !granted {
		t.Fatal("manual retry was not granted")
	}
	if err := manual.begin(); err != nil {
		t.Fatal(err)
	}
	replay, granted := load("first-click")
	if granted {
		t.Fatal("same click granted twice")
	}
	if err := replay.begin(); err == nil {
		t.Fatal("manual retry restored unlimited automatic trials")
	}
	repaired, err := loadAttemptsForTest(root, "v0.7.1", 2)
	if err != nil || repaired.record.Attempts != 0 {
		t.Fatalf("new configuration did not get a fresh budget: %v", err)
	}
}

func loadAttemptsForTest(root, version string, revision int64) (*attemptLedger, error) {
	ledger, _, err := loadAttempts(root, version, revision, "")
	return ledger, err
}

func TestLegacyPendingTrialGetsOneRecoveryAttempt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed deployments require Unix symlinks")
	}
	root, _, _ := releaseFixture(t)
	if err := os.Mkdir(filepath.Join(root, "deployments", "v0.7.1-r1"), 0o700); err != nil {
		t.Fatal(err)
	}
	ledger, _, err := loadAttempts(root, "v0.7.1", 1, "")
	if err != nil || ledger.record.Attempts != 1 {
		t.Fatalf("legacy pending attempt=%v err=%v", ledger, err)
	}
	if err := ledger.begin(); err != nil {
		t.Fatal(err)
	}
	ledger, _, err = loadAttempts(root, "v0.7.1", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.begin(); err == nil {
		t.Fatal("legacy target retried more than once")
	}
}

func TestCorruptAttemptRecordCannotBeResetByManualCheck(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "maintenance-attempt.json")
	for _, content := range []string{`{`, `{"schema":1,"target":"v1.0.0-r1","attempts":-1,"retry_id":""}`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadAttempts(root, "v1.0.0", 1, "manual"); err == nil {
			t.Fatal("corrupt evidence reset")
		}
	}
}

func TestManualNotificationSurvivesQueuedAutomaticCheck(t *testing.T) {
	queue := make(chan Trigger, 1)
	Notify(queue, Trigger{})
	Notify(queue, Trigger{RetryID: "manual"})
	Notify(queue, Trigger{})
	if event := <-queue; event.RetryID != "manual" {
		t.Fatal("automatic poll displaced manual retry")
	}
}
