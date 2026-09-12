package autoupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
)

// Opt-in measurement: real HTTPS polling/signatures and the production maintenance
// loop. CPU/RSS include the test runner and fake Cloud in this same process; this
// is deliberately not advertised as whole-Agent or production measurements.
func TestMaintenanceIdleResourceProbe(t *testing.T) {
	if os.Getenv("AKASTR_RESOURCE_PROBE") != "1" {
		t.Skip("opt-in 60-second resource measurement")
	}
	root, configRoot, current := releaseFixture(t)
	if err := os.Symlink(current, filepath.Join(current, "previous")); err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentials := identity.Identity{SchemaVersion: identity.SchemaVersion, EnrollmentState: identity.EnrollmentConfirmed, AgentID: "123e4567-e89b-42d3-a456-426614174000", PublicKey: base64.RawURLEncoding.EncodeToString(public), PrivateKey: base64.RawURLEncoding.EncodeToString(private)}
	manifest := manifestForApply(strings.Repeat("a", 64))
	manifest.Status = "current"
	manifest.Software.Status = "current"
	var checks, waits atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/internal/agents/maintenance-wait" {
			waits.Add(1)
			timer := time.NewTimer(25 * time.Second)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
				return
			case <-timer.C:
			}
			_, _ = io.WriteString(w, `{"check":false}`)
		} else if r.URL.Path == "/internal/agents/maintenance" {
			checks.Add(1)
			_ = json.NewEncoder(w).Encode(manifest)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := Client{HTTPClient: server.Client()}
	endpoint := "wss" + strings.TrimPrefix(server.URL, "https") + "/internal/agents/ws"
	triggers := make(chan Trigger, 1)
	watchDone := make(chan struct{})
	loopDone := make(chan error, 1)
	go func() { defer close(watchDone); client.Watch(ctx, endpoint, "v0.7.1", 1, credentials, triggers) }()
	go func() {
		loopDone <- RunLoop(ctx, LoopOptions{ControlEndpoint: endpoint, CurrentVersion: "v0.7.1", ConfigurationRevision: 1, Credentials: credentials, ConfigPath: filepath.Join(configRoot, "1", "config.json"), ConfigurationRoot: configRoot, ReleaseRoot: root, Lifecycle: lifecycle.New(), Client: client, Triggers: triggers, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Reexec: func(string, string, string, int64) error { return nil }})
	}()
	time.Sleep(2 * time.Second)
	var before, after syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &before)
	started := time.Now()
	time.Sleep(60 * time.Second)
	elapsed := time.Since(started).Seconds()
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &after)
	seconds := func(r syscall.Rusage) float64 {
		return float64(r.Utime.Sec+r.Stime.Sec) + float64(r.Utime.Usec+r.Stime.Usec)/1e6
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	status, _ := os.ReadFile("/proc/self/status")
	rss := "unavailable"
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			rss = strings.TrimSpace(strings.TrimPrefix(line, "VmRSS:"))
		}
	}
	t.Logf("maintenance_probe seconds=%.1f cpu_one_core_percent=%.3f rss=%s go_heap_bytes=%d checks=%d wait_requests=%d", elapsed, 100*(seconds(after)-seconds(before))/elapsed, rss, mem.HeapAlloc, checks.Load(), waits.Load())
	cancel()
	<-watchDone
	<-loopDone
	if checks.Load() != 2 || waits.Load() != 3 {
		t.Fatalf("unexpected idle request counts: checks=%d waits=%d", checks.Load(), waits.Load())
	}
}
