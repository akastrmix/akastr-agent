package ws

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/features/ipwatch"
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/coder/websocket"
)

type changingAddressObserver struct{ calls int }

func (o *changingAddressObserver) Observe(context.Context, ipwatch.Family) (ipwatch.Observation, error) {
	o.calls++
	address := "8.8.8.8"
	if o.calls > 1 {
		address = "8.8.4.4"
	}
	return ipwatch.Observation{Address: netip.MustParseAddr(address), ObservedAt: time.Now().UTC()}, nil
}

// Use the production heartbeat deadlines here: a peer accepts writes but
// suppresses Pongs and event ACKs. The actual client must reconnect and replay
// the durable event, without needing an incoming command to wake it up.
func TestClientReconnectsAndReplaysIPAfterHeartbeatFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 55*time.Second)
	defer cancel()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentials := identity.Identity{
		SchemaVersion: identity.SchemaVersion, EnrollmentState: identity.EnrollmentConfirmed,
		AgentID:   "f40a6d7e-bc54-4c8a-a68f-9895674677b6",
		PublicKey: base64.RawURLEncoding.EncodeToString(public), PrivateKey: base64.RawURLEncoding.EncodeToString(private),
	}
	statePath := filepath.Join(t.TempDir(), "ip.json")
	monitor, err := ipwatch.OpenMonitor(statePath, &changingAddressObserver{}, 10*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	// Start from an acknowledged baseline, as on an already-running TIME node.
	baselineContext, cancelBaseline := context.WithCancel(ctx)
	err = monitor.Run(baselineContext, func(body protocol.IPSnapshotBody) error {
		defer cancelBaseline()
		return monitor.AckSnapshot(body.SnapshotID)
	}, func(protocol.IPObservationBody) error { return nil }, func(protocol.ChangeIPUnchangedBody) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("baseline setup: %v", err)
	}
	var connections atomic.Int32
	events := make(chan protocol.IPObservationBody, 16)
	replayed := make(chan protocol.IPObservationBody, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		number := connections.Add(1)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OnPingReceived: func(context.Context, []byte) bool { return number > 1 },
		})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		s := &session{connection: conn}
		now := time.Now().UTC()
		challenge := protocol.AuthChallenge{
			ChallengeID: "123e4567-e89b-42d3-a456-426614174000", AgentID: credentials.AgentID,
			Nonce:    base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
			IssuedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
		}
		if s.write(ctx, "auth.challenge", challenge) != nil {
			return
		}
		if _, err := readEnvelope(ctx, conn, "auth.response"); err != nil {
			return
		}
		if s.write(ctx, "auth.accepted", protocol.AgentIDBody{AgentID: credentials.AgentID}) != nil {
			return
		}
		if _, err := readEnvelope(ctx, conn, "agent.hello"); err != nil {
			return
		}
		if s.write(ctx, "hello.accepted", protocol.AgentIDBody{AgentID: credentials.AgentID}) != nil {
			return
		}
		for {
			envelope, err := readProtocolEnvelope(ctx, conn)
			if err != nil {
				return
			}
			switch envelope.Type {
			case "ip.snapshot":
				body, err := protocol.DecodeBody[protocol.IPSnapshotBody](envelope, "snapshot_id", "family", "address", "observed_at")
				if err != nil {
					return
				}
				if s.write(ctx, "ip.snapshot_ack", protocol.IPSnapshotAckBody{SnapshotID: body.SnapshotID, Persisted: true}) != nil {
					return
				}
			case "ip.observed":
				body, err := protocol.DecodeBody[protocol.IPObservationBody](envelope, "observation_id", "family", "previous_address", "address", "observed_at")
				if err != nil {
					return
				}
				if number == 1 {
					events <- body
					continue
				}
				if s.write(ctx, "ip.observed_ack", protocol.IPObservationAckBody{ObservationID: body.ObservationID, Persisted: true}) != nil {
					return
				}
				replayed <- body
			}
		}
	}))
	defer server.Close()
	executor := &recordingExecutor{executed: make(chan string, 1)}
	client := &Client{
		endpoint: strings.Replace(server.URL, "http", "ws", 1), identity: credentials,
		version: "v1.6.1", configurationRevision: 1, deploymentState: "current",
		executor: executor, observations: monitor, lifecycle: lifecycle.New(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		running: make(map[string]*lifecycle.Lease), pending: make(map[string]pendingOperation),
	}
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	defer func() { cancel(); <-done }()
	var first, replay protocol.IPObservationBody
	select {
	case first = <-events:
	case <-ctx.Done():
		t.Fatal("no initial IP event")
	}
	select {
	case replay = <-replayed:
	case <-ctx.Done():
		t.Fatal("client failed to reconnect and replay")
	}
	if first != replay || replay.Address != "8.8.4.4" || connections.Load() != 2 {
		t.Fatalf("event changed during recovery: first=%+v replay=%+v connections=%d", first, replay, connections.Load())
	}
	// Wait for the real monitor to persist the ACK, not just a successful write.
	for ipwatch.CheckIdle(statePath) != nil {
		select {
		case <-ctx.Done():
			t.Fatal("IP ACK was not persisted")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case <-executor.executed:
		t.Fatal("connection recovery executed an unsolicited operation")
	default:
	}
}

func TestConnectionWatchDetectsMissingPong(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var pings atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OnPingReceived: func(context.Context, []byte) bool {
				pings.Add(1)
				return false // Socket accepts writes, but no round trip completes.
			},
		})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.Read(ctx)
	}))
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	stop := (&session{connection: conn}).watchConnection(ctx, 10*time.Millisecond, 100*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer stop()
	_, _, err = conn.Read(ctx)
	if err == nil || ctx.Err() != nil || pings.Load() == 0 {
		t.Fatalf("watchdog did not close unresponsive connection: err=%v context=%v pings=%d", err, ctx.Err(), pings.Load())
	}
}

func TestConnectionWatchKeepsHealthyConnectionAndStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	pings := make(chan struct{}, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OnPingReceived: func(context.Context, []byte) bool {
				select {
				case pings <- struct{}{}:
				default:
				}
				return true
			},
		})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.Read(ctx)
	}))
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	readDone := make(chan struct{})
	go func() { defer close(readDone); conn.Read(ctx) }()
	stop := (&session{connection: conn}).watchConnection(ctx, 10*time.Millisecond, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for range 3 {
		select {
		case <-pings:
		case <-readDone:
			t.Fatal("healthy connection was closed")
		case <-ctx.Done():
			t.Fatal("heartbeat did not run")
		}
	}
	stop()
	// The stop function joins its goroutine; closing the session cannot leave a
	// watchdog behind to interfere with a replacement connection.
	conn.CloseNow()
	select {
	case <-readDone:
	case <-ctx.Done():
		t.Fatal("reader leaked")
	}
}

func TestConnectionSetupTimesOut(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrade", true: "authentication"}[upgrade], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if upgrade {
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer conn.CloseNow()
				}
				<-release
			}))
			defer server.Close()
			defer close(release)
			client := &Client{endpoint: strings.Replace(server.URL, "http", "ws", 1)}
			err := client.runSessionWithTimeout(ctx, 100*time.Millisecond)
			if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				t.Fatalf("setup did not enforce its own deadline: %v", err)
			}
		})
	}
}
