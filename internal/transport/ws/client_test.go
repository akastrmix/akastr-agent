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
	"strings"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/feature"
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/coder/websocket"
)

// baseRuntime is a node with no module behaviour of interest to a test.
type baseRuntime struct{}

func (baseRuntime) Accepting(protocol.OperationOffer) (bool, error) { return true, nil }
func (baseRuntime) Execute(context.Context, protocol.OperationOffer) (protocol.ExecutionResult, error) {
	return protocol.ExecutionResult{}, errors.New("unexpected execution")
}
func (baseRuntime) Run(ctx context.Context, _ feature.Publish) error { <-ctx.Done(); return ctx.Err() }
func (baseRuntime) ControlReady()                                    {}
func (baseRuntime) Handle(protocol.Envelope) (bool, error)           { return false, nil }

type recordingExecutor struct {
	baseRuntime
	executed chan string
}

func TestReconnectBackoffResetsOnlyAfterStableSession(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name    string
		readyAt time.Time
		want    time.Duration
	}{
		{"never authenticated", time.Time{}, 30 * time.Second},
		{"flapping", now.Add(-time.Second), 30 * time.Second},
		{"healthy then disconnected", now.Add(-heartbeatInterval), time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sessionBackoff(30*time.Second, test.readyAt, now, heartbeatInterval); got != test.want {
				t.Fatalf("backoff=%v, want %v", got, test.want)
			}
		})
	}
}

type blockingExecutor struct {
	baseRuntime
	started chan struct{}
	release chan struct{}
}

type fatalExecutor struct{ baseRuntime }

func (fatalExecutor) Execute(context.Context, protocol.OperationOffer) (protocol.ExecutionResult, error) {
	return protocol.ExecutionResult{}, errors.New("durable state unavailable")
}

func (e *blockingExecutor) Execute(context.Context, protocol.OperationOffer) (protocol.ExecutionResult, error) {
	close(e.started)
	<-e.release
	return protocol.ExecutionResult{Outcome: "failed", Code: "test", Result: map[string]any{}}, nil
}

type failingRuntime struct{ baseRuntime }

func (failingRuntime) Run(context.Context, feature.Publish) error {
	return errors.New("observation state failed")
}

// holdingRuntime has a module that does not take new work yet.
type holdingRuntime struct{ baseRuntime }

func (holdingRuntime) Accepting(protocol.OperationOffer) (bool, error) { return false, nil }

type rejectingRuntime struct{ baseRuntime }

func (rejectingRuntime) Accepting(protocol.OperationOffer) (bool, error) {
	return false, errors.New("command type is not enabled on this node")
}

func TestOperationLeaseBlocksUpdateUntilExecutionFinishes(t *testing.T) {
	gate := lifecycle.New()
	executor := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	client := &Client{
		runtime: executor, lifecycle: gate, running: map[string]*lifecycle.Lease{},
		pending: map[string]pendingOperation{},
	}
	offer := protocol.OperationOffer{
		CommandID: "123e4567-e89b-42d3-a456-426614174002",
		NotBefore: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Minute),
	}
	lease, _ := gate.TryOperation()
	client.pending[offer.CommandID] = pendingOperation{offer: offer, lease: lease}
	client.handleAcceptedAck(t.Context(), protocol.AcceptedAckBody{CommandID: offer.CommandID, Accepted: true})
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("operation did not start")
	}
	if update, ok := gate.TryUpdate(); ok || update != nil {
		t.Fatal("update acquired while an accepted operation was executing")
	}
	close(executor.release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if update, ok := gate.TryUpdate(); ok {
			update.Release()
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("operation lease was not released after terminal persistence")
}

func TestExecutionPersistenceFailureBecomesFatalWithoutWireResult(t *testing.T) {
	gate := lifecycle.New()
	lease, ok := gate.TryOperation()
	if !ok {
		t.Fatal("operation lease was rejected")
	}
	commandID := "123e4567-e89b-42d3-a456-426614174003"
	client := &Client{
		runtime: fatalExecutor{}, lifecycle: gate,
		running: map[string]*lifecycle.Lease{commandID: lease},
		pending: make(map[string]pendingOperation),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	client.execute(t.Context(), protocol.OperationOffer{CommandID: commandID})
	if client.executionFailure() == nil {
		t.Fatal("executor persistence failure did not stop the control loop")
	}
	if update, ok := gate.TryUpdate(); !ok {
		t.Fatal("operation lease was not released after fatal execution failure")
	} else {
		update.Release()
	}
}

func TestFatalObservationErrorStopsClient(t *testing.T) {
	client := &Client{
		endpoint: "wss://127.0.0.1:1/internal/agents/ws",
		identity: identity.Identity{AgentID: "123e4567-e89b-42d3-a456-426614174000"},
		runtime:  failingRuntime{},
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := client.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "Agent module failed") {
		t.Fatalf("Run error = %v, want fatal monitor failure", err)
	}
}

func TestReadyCallbackRunsAfterHelloAccepted(t *testing.T) {
	agentID := "f40a6d7e-bc54-4c8a-a68f-9895674677b6"
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentials := identity.Identity{
		SchemaVersion: identity.SchemaVersion, AgentID: agentID, PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
		PrivateKey: base64.RawURLEncoding.EncodeToString(privateKey),
	}
	helloAcknowledged := make(chan struct{})
	ready := make(chan struct{})
	serverErrors := make(chan error, 1)
	serverDone := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		defer close(serverDone)
		connection, acceptError := websocket.Accept(response, request, nil)
		if acceptError != nil {
			serverErrors <- acceptError
			return
		}
		defer connection.CloseNow()
		session := &session{connection: connection}
		now := time.Now().UTC()
		challenge := protocol.AuthChallenge{
			ChallengeID: "123e4567-e89b-42d3-a456-426614174000", AgentID: agentID,
			Nonce:     base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
			IssuedAt:  now.Add(-time.Second).Format(time.RFC3339Nano),
			ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
		}
		if writeError := session.write(request.Context(), "auth.challenge", challenge); writeError != nil {
			serverErrors <- writeError
			return
		}
		if _, readError := readEnvelope(request.Context(), connection, "auth.response"); readError != nil {
			serverErrors <- readError
			return
		}
		if writeError := session.write(request.Context(), "auth.accepted", protocol.AgentIDBody{AgentID: agentID}); writeError != nil {
			serverErrors <- writeError
			return
		}
		helloEnvelope, readError := readEnvelope(request.Context(), connection, "agent.hello")
		if readError != nil {
			serverErrors <- readError
			return
		}
		if _, decodeError := protocol.DecodeBody[protocol.HelloBody](
			helloEnvelope, "agent_version", "configuration_revision", "capabilities",
		); decodeError != nil {
			serverErrors <- decodeError
			return
		}
		close(helloAcknowledged)
		if writeError := session.write(request.Context(), "hello.accepted", protocol.AgentIDBody{AgentID: agentID}); writeError != nil {
			serverErrors <- writeError
			return
		}
		<-ready
	}))
	defer server.Close()
	originalTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = originalTransport }()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client, err := New(Options{
		Endpoint: strings.Replace(server.URL, "https://", "wss://", 1) + "/internal/agents/ws",
		Identity: credentials, Version: "v1.4.0", ConfigurationRevision: 2,
		Capabilities: []capability.Descriptor{},
		Runtime:      &recordingExecutor{executed: make(chan string, 1)}, Lifecycle: lifecycle.New(),
		OnReady: func() error {
			select {
			case <-helloAcknowledged:
			default:
				return errors.New("ready callback ran before hello.accepted")
			}
			close(ready)
			cancel()
			return nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.runSession(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("runSession() error = %v", err)
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("control server did not finish")
	}
	select {
	case serverError := <-serverErrors:
		t.Fatal(serverError)
	default:
	}
}

func (e *recordingExecutor) Execute(_ context.Context, offer protocol.OperationOffer) (protocol.ExecutionResult, error) {
	e.executed <- offer.CommandID
	return protocol.ExecutionResult{Outcome: "failed", Code: "test", Result: map[string]any{}}, nil
}

func TestAcceptedAckGatesExecution(t *testing.T) {
	executor := &recordingExecutor{executed: make(chan string, 2)}
	client := &Client{
		runtime: executor, lifecycle: lifecycle.New(), running: map[string]*lifecycle.Lease{},
		pending: map[string]pendingOperation{},
	}
	first := protocol.OperationOffer{
		CommandID: "123e4567-e89b-42d3-a456-426614174000",
		ExpiresAt: time.Now().Add(time.Minute),
	}
	firstLease, _ := client.lifecycle.TryOperation()
	client.pending[first.CommandID] = pendingOperation{offer: first, lease: firstLease}
	client.handleAcceptedAck(context.Background(), protocol.AcceptedAckBody{
		CommandID: first.CommandID, Accepted: false,
	})
	select {
	case id := <-executor.executed:
		t.Fatalf("rejected command executed: %s", id)
	case <-time.After(20 * time.Millisecond):
	}

	second := first
	second.CommandID = "123e4567-e89b-42d3-a456-426614174001"
	secondLease, _ := client.lifecycle.TryOperation()
	client.pending[second.CommandID] = pendingOperation{offer: second, lease: secondLease}
	client.handleAcceptedAck(context.Background(), protocol.AcceptedAckBody{
		CommandID: second.CommandID, Accepted: true,
	})
	select {
	case id := <-executor.executed:
		if id != second.CommandID {
			t.Fatalf("executed %s", id)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted command was not executed")
	}
}

func TestExpiredOfferCanReachAuthoritativeAcceptanceHandshake(t *testing.T) {
	now := time.Now()
	offer := protocol.OperationOffer{
		NotBefore: now.Add(-2 * time.Minute), ExpiresAt: now.Add(-time.Minute),
	}
	if !offerHandshakeAllows(offer, now) {
		t.Fatal("expired command was blocked before the Cloud acceptance acknowledgement")
	}
}

func TestOfferWaitsWhileItsModuleIsNotAccepting(t *testing.T) {
	client := &Client{
		runtime:   holdingRuntime{},
		lifecycle: lifecycle.New(), running: map[string]*lifecycle.Lease{},
		pending: map[string]pendingOperation{},
	}
	err := client.acceptOffer(t.Context(), nil, protocol.OperationOffer{
		CommandID:   "123e4567-e89b-42d3-a456-426614174005",
		CommandType: "changeip.execute",
		NotBefore:   time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("acceptOffer() error = %v", err)
	}
	if len(client.pending) != 0 || len(client.running) != 0 {
		t.Fatal("offer was accepted while its module was not accepting")
	}
	update, acquired := client.lifecycle.TryUpdate()
	if !acquired {
		t.Fatal("ignored offer retained an operation lease")
	}
	update.Release()
}

func TestOfferDuringAutomaticUpdateIsDeferredWithoutClosingTheSession(t *testing.T) {
	gate := lifecycle.New()
	update, acquired := gate.TryUpdate()
	if !acquired {
		t.Fatal("update lease was rejected")
	}
	defer update.Release()
	client := &Client{
		runtime: &recordingExecutor{}, lifecycle: gate,
		running: map[string]*lifecycle.Lease{}, pending: map[string]pendingOperation{},
	}
	err := client.acceptOffer(t.Context(), nil, protocol.OperationOffer{
		CommandID:   "123e4567-e89b-42d3-a456-426614174007",
		CommandType: "ipquality.execute",
		NotBefore:   time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("offer during update closed the session: %v", err)
	}
	if len(client.pending) != 0 {
		t.Fatal("offer during update was accepted")
	}
}

func TestOfferForDisabledModuleClosesTheSession(t *testing.T) {
	client := &Client{
		runtime: rejectingRuntime{}, lifecycle: lifecycle.New(),
		running: map[string]*lifecycle.Lease{}, pending: map[string]pendingOperation{},
	}
	err := client.acceptOffer(t.Context(), nil, protocol.OperationOffer{
		CommandID:   "123e4567-e89b-42d3-a456-426614174006",
		CommandType: "changeip.execute",
		NotBefore:   time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Minute),
	})
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("acceptOffer() error = %v", err)
	}
}

func TestAcceptedAckExecutesAfterOfferExpiry(t *testing.T) {
	executor := &recordingExecutor{executed: make(chan string, 1)}
	client := &Client{
		runtime: executor, lifecycle: lifecycle.New(), running: map[string]*lifecycle.Lease{},
		pending: map[string]pendingOperation{},
	}
	offer := protocol.OperationOffer{
		CommandID: "123e4567-e89b-42d3-a456-426614174004",
		NotBefore: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(-time.Second),
	}
	lease, _ := client.lifecycle.TryOperation()
	client.pending[offer.CommandID] = pendingOperation{offer: offer, lease: lease}
	client.handleAcceptedAck(t.Context(), protocol.AcceptedAckBody{
		CommandID: offer.CommandID, Accepted: true,
	})
	select {
	case commandID := <-executor.executed:
		if commandID != offer.CommandID {
			t.Fatalf("executed %s", commandID)
		}
	case <-time.After(time.Second):
		t.Fatal("Cloud-accepted expired offer was not executed")
	}
}
