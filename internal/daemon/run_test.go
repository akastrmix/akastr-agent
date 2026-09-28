package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/layout"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/coder/websocket"
)

func waitForCancel(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

// A candidate that Cloud never accepts must exit so systemd restarts the
// previous deployment, and it must not start updating itself meanwhile.
func TestCandidateStopsAtDeadlineWithoutUpdating(t *testing.T) {
	var updating atomic.Bool
	err := supervise(t.Context(), waitForCancel,
		func(ctx context.Context) error { updating.Store(true); return waitForCancel(ctx) },
		make(chan struct{}), 20*time.Millisecond)
	if !errors.Is(err, errCandidateTimeout) || updating.Load() {
		t.Fatalf("err=%v updating=%v", err, updating.Load())
	}
}

func TestActivatedCandidateStartsUpdatesAndDisarmsDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	committed, started := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- supervise(ctx,
			func(ctx context.Context) error { close(committed); return waitForCancel(ctx) },
			func(ctx context.Context) error { close(started); return waitForCancel(ctx) },
			committed, 20*time.Millisecond)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("activated candidate did not start updates")
	}
	select {
	case err := <-done:
		t.Fatalf("activated candidate stopped: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The whole candidate path through Run: a process started from the inactive
// slot becomes the active deployment only once Cloud accepts its hello.
func TestCandidateActivatesItsSlotWhenCloudAcceptsIt(t *testing.T) {
	root := t.TempDir()
	paths := layout.Layout{Root: filepath.Join(root, "lib"), StateDir: filepath.Join(root, "state"), IdentityFile: filepath.Join(root, "identity.json")}
	credentials, err := identity.Generate("123e4567-e89b-42d3-a456-426614174102")
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.Save(paths.IdentityFile); err != nil {
		t.Fatal(err)
	}
	for _, slot := range []string{paths.Slot("a"), paths.Slot("b")} {
		if err := os.MkdirAll(slot, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := paths.Activate(paths.Slot("a")); err != nil {
		t.Fatal(err)
	}
	hellos := make(chan protocol.HelloBody, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/internal/agents/ws" {
			http.NotFound(response, request)
			return
		}
		connection, err := websocket.Accept(response, request, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		send := func(messageType string, body any) error {
			encoded, err := protocol.Encode(messageType, body)
			if err != nil {
				return err
			}
			return connection.Write(request.Context(), websocket.MessageText, encoded)
		}
		read := func() (protocol.Envelope, error) {
			_, data, err := connection.Read(request.Context())
			if err != nil {
				return protocol.Envelope{}, err
			}
			return protocol.Decode(data)
		}
		now := time.Now().UTC()
		if send("auth.challenge", protocol.AuthChallenge{
			ChallengeID: "123e4567-e89b-42d3-a456-426614174000", AgentID: credentials.AgentID,
			Nonce:    base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
			IssuedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
		}) != nil {
			return
		}
		if _, err := read(); err != nil || send("auth.accepted", protocol.AgentIDBody{AgentID: credentials.AgentID}) != nil {
			return
		}
		envelope, err := read()
		if err != nil {
			return
		}
		hello, err := protocol.DecodeBody[protocol.HelloBody](envelope, "agent_version", "configuration_revision", "capabilities")
		if err != nil {
			return
		}
		hellos <- hello
		if send("hello.accepted", protocol.AgentIDBody{AgentID: credentials.AgentID}) != nil {
			return
		}
		for {
			if _, err := read(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	originalTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = originalTransport }()
	configuration := fmt.Sprintf(`{"schema_version":4,"configuration_revision":4,"mode":"target",
"agent_id":%q,"name":"HKT","control_endpoint":"wss://%s/internal/agents/ws",
"target":{"ip_watch_interval_seconds":60,"observe_ipv6":false,"change_ip":{"provider":"disabled"},"socks5":{"enabled":false}}}`,
		credentials.AgentID, strings.TrimPrefix(server.URL, "https://"))
	configPath := layout.SlotConfig(paths.Slot("b"))
	if err := os.WriteFile(configPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			ConfigPath: configPath, Version: "v1.8.0", Layout: paths,
			Exec:   func(string, string) error { return errors.New("unexpected update") },
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()
	select {
	case hello := <-hellos:
		if hello.AgentVersion != "v1.8.0" || hello.ConfigurationRevision != 4 {
			t.Fatalf("hello = %+v", hello)
		}
	case err := <-done:
		t.Fatalf("candidate stopped before its hello: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("candidate did not send hello")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if active, _ := paths.ActiveSlot(); active == paths.Slot("b") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("accepted candidate did not activate its slot")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
