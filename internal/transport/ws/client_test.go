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
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/coder/websocket"
)

type recordingExecutor struct {
	executed chan string
}

type blockingExecutor struct {
	started chan struct{}
	release chan struct{}
}

type fatalExecutor struct{}

func (fatalExecutor) Execute(context.Context, protocol.OperationOffer) (protocol.ExecutionResult, error) {
	return protocol.ExecutionResult{}, errors.New("durable state unavailable")
}

func (e *blockingExecutor) Execute(context.Context, protocol.OperationOffer) (protocol.ExecutionResult, error) {
	close(e.started)
	<-e.release
	return protocol.ExecutionResult{Outcome: "failed", Code: "test", Result: map[string]any{}}, nil
}

type failingObservationSource struct{}

func (failingObservationSource) Run(context.Context, func(protocol.IPSnapshotBody) error, func(protocol.IPObservationBody) error, func(protocol.ChangeIPUnchangedBody) error) error {
	return errors.New("observation state failed")
}

func (failingObservationSource) NotifyControlReady()       {}
func (failingObservationSource) SnapshotReady() bool       { return false }
func (failingObservationSource) AckSnapshot(string) error  { return nil }
func (failingObservationSource) Ack(string) error          { return nil }
func (failingObservationSource) AckUnchanged(string) error { return nil }

type nilObservationSource struct{}

type readinessObservationSource struct{ ready bool }

func (source readinessObservationSource) Run(context.Context, func(protocol.IPSnapshotBody) error, func(protocol.IPObservationBody) error, func(protocol.ChangeIPUnchangedBody) error) error {
	return nil
}
func (readinessObservationSource) NotifyControlReady()        {}
func (source readinessObservationSource) SnapshotReady() bool { return source.ready }
func (readinessObservationSource) AckSnapshot(string) error   { return nil }
func (readinessObservationSource) Ack(string) error           { return nil }
func (readinessObservationSource) AckUnchanged(string) error  { return nil }

func (*nilObservationSource) Run(context.Context, func(protocol.IPSnapshotBody) error, func(protocol.IPObservationBody) error, func(protocol.ChangeIPUnchangedBody) error) error {
	return nil
}

func TestOperationLeaseBlocksUpdateUntilExecutionFinishes(t *testing.T) {
	gate := lifecycle.New()
	executor := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	client := &Client{
		executor: executor, lifecycle: gate, running: map[string]*lifecycle.Lease{},
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
		executor: fatalExecutor{}, lifecycle: gate,
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
		endpoint:     "wss://127.0.0.1:1/internal/agents/ws",
		identity:     identity.Identity{AgentID: "123e4567-e89b-42d3-a456-426614174000"},
		observations: failingObservationSource{},
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := client.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "IP observation monitor failed") {
		t.Fatalf("Run error = %v, want fatal monitor failure", err)
	}
}

func (*nilObservationSource) AckSnapshot(string) error  { return nil }
func (*nilObservationSource) Ack(string) error          { return nil }
func (*nilObservationSource) AckUnchanged(string) error { return nil }
func (*nilObservationSource) NotifyControlReady()       {}
func (*nilObservationSource) SnapshotReady() bool       { return false }

func TestNewRejectsTypedNilObservationSource(t *testing.T) {
	var observations *nilObservationSource
	_, err := New(struct {
		Endpoint              string
		Identity              identity.Identity
		Version               string
		ConfigurationRevision int64
		Capabilities          []capability.Descriptor
		DeploymentState       string
		Executor              Executor
		Observations          ObservationSource
		Lifecycle             *lifecycle.Gate
		OnReady               func() error
		OnDeploymentTrial     func() error
		OnMaintenanceCheck    func()
		Logger                *slog.Logger
	}{
		Endpoint: "wss://control.example/internal/agents/ws", ConfigurationRevision: 1,
		DeploymentState: "current",
		Executor:        &recordingExecutor{},
		Observations:    observations, Lifecycle: lifecycle.New(),
	})
	if err == nil || !strings.Contains(err.Error(), "nil implementation") {
		t.Fatalf("New() error = %v, want typed nil rejection", err)
	}
}

func TestMaintenanceCheckTriggersExistingReconciliationLoop(t *testing.T) {
	triggered := 0
	client := &Client{onMaintenanceCheck: func() { triggered++ }}
	encoded, err := protocol.Encode("maintenance.check", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := protocol.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.handleMaintenanceCheck(envelope); err != nil {
		t.Fatalf("handleMaintenanceCheck() error = %v", err)
	}
	if triggered != 1 {
		t.Fatalf("manual maintenance triggers = %d, want 1", triggered)
	}
	envelope.Body = []byte(`{"unexpected":true}`)
	if err := client.handleMaintenanceCheck(envelope); err == nil {
		t.Fatal("maintenance.check accepted an unknown body field")
	}
}

func TestMaintenanceOnlySessionRejectsBusinessMessages(t *testing.T) {
	client := &Client{onMaintenanceCheck: func() {}}
	encoded, err := protocol.Encode("operation.offer", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := protocol.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.handleMaintenanceSessionEnvelope(envelope); err == nil {
		t.Fatal("maintenance-only session accepted a business message")
	}
}

func TestHelloResponseSelectsMaintenanceOnlySession(t *testing.T) {
	agentID := "f40a6d7e-bc54-4c8a-a68f-9895674677b6"
	encoded, err := protocol.Encode("maintenance.required", protocol.AgentIDBody{AgentID: agentID})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := protocol.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	mode, err := helloSessionMode(envelope, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if mode != sessionMaintenance {
		t.Fatalf("maintenance response selected session mode %d", mode)
	}
	if _, err := helloSessionMode(envelope, "2bfadfbb-7481-4d96-9e0b-40a04aa4aeb4"); err == nil {
		t.Fatal("maintenance response accepted another Agent identity")
	}
}

func TestHelloResponseSelectsDeploymentTrial(t *testing.T) {
	agentID := "f40a6d7e-bc54-4c8a-a68f-9895674677b6"
	encoded, err := protocol.Encode("deployment.trial_accepted", protocol.AgentIDBody{AgentID: agentID})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := protocol.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	mode, err := helloSessionMode(envelope, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if mode != sessionTrial {
		t.Fatalf("trial response selected session mode %d", mode)
	}
}

func TestDeploymentTrialCommitsBeforeReady(t *testing.T) {
	agentID := "f40a6d7e-bc54-4c8a-a68f-9895674677b6"
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentials := identity.Identity{
		SchemaVersion: identity.SchemaVersion, EnrollmentState: identity.EnrollmentConfirmed,
		AgentID: agentID, PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
		PrivateKey: base64.RawURLEncoding.EncodeToString(privateKey),
	}
	trialCommitted := make(chan struct{})
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
		hello, decodeError := protocol.DecodeBody[protocol.HelloBody](
			helloEnvelope, "agent_version", "configuration_revision", "deployment_state", "capabilities",
		)
		if decodeError != nil || hello.DeploymentState != "trial" {
			serverErrors <- errors.New("client did not authenticate as a deployment trial")
			return
		}
		if writeError := session.write(request.Context(), "deployment.trial_accepted", protocol.AgentIDBody{AgentID: agentID}); writeError != nil {
			serverErrors <- writeError
			return
		}
		committed, readError := readEnvelope(request.Context(), connection, "deployment.committed")
		if readError != nil {
			serverErrors <- readError
			return
		}
		if _, decodeError := protocol.DecodeBody[struct{}](committed); decodeError != nil {
			serverErrors <- decodeError
			return
		}
		select {
		case <-trialCommitted:
		default:
			serverErrors <- errors.New("deployment.committed was sent before the local trial callback")
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
	trialCalls := 0
	client, err := New(struct {
		Endpoint              string
		Identity              identity.Identity
		Version               string
		ConfigurationRevision int64
		Capabilities          []capability.Descriptor
		DeploymentState       string
		Executor              Executor
		Observations          ObservationSource
		Lifecycle             *lifecycle.Gate
		OnReady               func() error
		OnDeploymentTrial     func() error
		OnMaintenanceCheck    func()
		Logger                *slog.Logger
	}{
		Endpoint: strings.Replace(server.URL, "https://", "wss://", 1) + "/internal/agents/ws",
		Identity: credentials, Version: "v1.4.0", ConfigurationRevision: 2,
		Capabilities: []capability.Descriptor{}, DeploymentState: "trial",
		Executor: &recordingExecutor{executed: make(chan string, 1)}, Lifecycle: lifecycle.New(),
		OnDeploymentTrial: func() error {
			trialCalls++
			close(trialCommitted)
			return nil
		},
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
		t.Fatal("trial server did not finish")
	}
	select {
	case serverError := <-serverErrors:
		t.Fatal(serverError)
	default:
	}
	if trialCalls != 1 || client.deploymentState != "current" {
		t.Fatalf("trial callbacks=%d deployment_state=%s", trialCalls, client.deploymentState)
	}
}

func (e *recordingExecutor) Execute(_ context.Context, offer protocol.OperationOffer) (protocol.ExecutionResult, error) {
	e.executed <- offer.CommandID
	return protocol.ExecutionResult{Outcome: "failed", Code: "test", Result: map[string]any{}}, nil
}

func TestAcceptedAckGatesExecution(t *testing.T) {
	executor := &recordingExecutor{executed: make(chan string, 2)}
	client := &Client{
		executor: executor, lifecycle: lifecycle.New(), running: map[string]*lifecycle.Lease{},
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

func TestChangeIPOfferWaitsForSnapshotAcknowledgement(t *testing.T) {
	client := &Client{
		executor: &recordingExecutor{}, observations: readinessObservationSource{ready: false},
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
		t.Fatal("ChangeIP offer was accepted before snapshot acknowledgement")
	}
	update, acquired := client.lifecycle.TryUpdate()
	if !acquired {
		t.Fatal("ignored ChangeIP offer retained an operation lease")
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
		executor: &recordingExecutor{}, lifecycle: gate,
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

func TestChangeIPOfferFailsWhenObservationSourceIsUnavailable(t *testing.T) {
	client := &Client{
		executor: &recordingExecutor{}, lifecycle: lifecycle.New(),
		running: map[string]*lifecycle.Lease{}, pending: map[string]pendingOperation{},
	}
	err := client.acceptOffer(t.Context(), nil, protocol.OperationOffer{
		CommandID:   "123e4567-e89b-42d3-a456-426614174006",
		CommandType: "changeip.execute",
		NotBefore:   time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Minute),
	})
	if err == nil || !strings.Contains(err.Error(), "observation source") {
		t.Fatalf("acceptOffer() error = %v", err)
	}
}

func TestAcceptedAckExecutesAfterOfferExpiry(t *testing.T) {
	executor := &recordingExecutor{executed: make(chan string, 1)}
	client := &Client{
		executor: executor, lifecycle: lifecycle.New(), running: map[string]*lifecycle.Lease{},
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
