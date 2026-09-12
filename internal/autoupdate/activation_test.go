package autoupdate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
)

type activationClient struct {
	reconciliationClient
	checks  int
	changed bool
	busy    bool
}

func (c *activationClient) Check(ctx context.Context, endpoint, version string, revision int64, id identity.Identity) (Manifest, error) {
	c.checks++
	m, err := c.reconciliationClient.Check(ctx, endpoint, version, revision, id)
	if c.changed && c.checks > 1 {
		m.Configuration.Revision++
	}
	if c.busy && c.checks > 1 {
		m.Status = "busy"
	}
	return m, err
}

func TestCandidatePreparationAllowsWorkAndActivationRechecksTarget(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("release activation requires Linux")
	}
	for _, scenario := range []struct {
		name          string
		targetChanged bool
		busy          bool
	}{
		{name: "unchanged"},
		{name: "target_changed", targetChanged: true},
		{name: "busy", busy: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root, configRoot, _ := releaseFixture(t)
			binary := "prepared-candidate"
			manifest := manifestForApply(fmt.Sprintf("%x", sha256.Sum256([]byte(binary))))
			client := &activationClient{reconciliationClient: reconciliationClient{manifest: &manifest}, changed: scenario.targetChanged, busy: scenario.busy}
			gate := lifecycle.New()
			now := time.Now()
			retry := &RetryState{now: func() time.Time { return now }}
			executed := false
			changed, err := ReconcileOnce(t.Context(), LoopOptions{
				ControlEndpoint: "wss://control.example/internal/agents/ws", CurrentVersion: "v0.7.0", ConfigurationRevision: 1,
				ConfigPath: filepath.Join(configRoot, "1", "config.json"), ReleaseRoot: root,
				Client: client, Runner: &fakeRunner{}, Lifecycle: gate, Retry: retry,
				Stage: func(ctx context.Context, options ApplyOptions) (StagedRelease, error) {
					lease, ok := gate.TryOperation()
					if !ok {
						t.Fatal("download unnecessarily blocked business execution")
					}
					lease.Release()
					options.HTTPClient = &http.Client{Transport: responseTransport{body: binary}}
					return Stage(ctx, options)
				},
				Reexec: func(string, string, string, int64) error {
					if lease, ok := gate.TryOperation(); ok {
						lease.Release()
						t.Fatal("activation did not exclude execution")
					}
					executed = true
					return nil
				},
			})
			wantActivation := !scenario.targetChanged && !scenario.busy
			if err != nil || changed != wantActivation || executed != wantActivation || client.checks != 2 {
				t.Fatalf("changed=%v executed=%v checks=%d err=%v", changed, executed, client.checks, err)
			}
			if !wantActivation {
				if !retry.next.Equal(now.Add(30 * time.Second)) {
					t.Fatalf("final target check did not schedule retry: %v", retry.next)
				}
				ledger, _, err := loadAttempts(root, manifest.Software.Version, manifest.Configuration.Revision, "")
				if err != nil || ledger.record.Attempts != 0 {
					t.Fatalf("final target check consumed trial budget: ledger=%v err=%v", ledger, err)
				}
			}
		})
	}
}
