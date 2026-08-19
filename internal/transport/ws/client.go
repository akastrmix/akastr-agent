package ws

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"reflect"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/coder/websocket"
)

type Executor interface {
	Execute(context.Context, protocol.OperationOffer) (protocol.ExecutionResult, error)
	KnownOperation(commandID, commandType string) bool
}

type ObservationSource interface {
	Run(context.Context, func(protocol.IPSnapshotBody) error, func(protocol.IPObservationBody) error, func(protocol.ChangeIPUnchangedBody) error) error
	AckSnapshot(string) error
	Ack(string) error
	AckUnchanged(string) error
}

type Client struct {
	endpoint     string
	identity     identity.Identity
	version      string
	capabilities []capability.Descriptor
	executor     Executor
	observations ObservationSource
	lifecycle    *lifecycle.Gate
	onReady      func() error
	logger       *slog.Logger

	mu        sync.Mutex
	active    *session
	running   map[string]*lifecycle.Lease
	pending   map[string]pendingOperation
	completed map[string]protocol.ExecutionResult
	fatalErr  error
}

type pendingOperation struct {
	offer    protocol.OperationOffer
	lease    *lifecycle.Lease
	recovery bool
}

type session struct {
	connection *websocket.Conn
	writeMu    sync.Mutex
}

func New(options struct {
	Endpoint     string
	Identity     identity.Identity
	Version      string
	Capabilities []capability.Descriptor
	Executor     Executor
	Observations ObservationSource
	Lifecycle    *lifecycle.Gate
	OnReady      func() error
	Logger       *slog.Logger
}) (*Client, error) {
	if options.Executor == nil {
		return nil, errors.New("WSS executor is required")
	}
	if options.Observations != nil && isNilObservationSource(options.Observations) {
		return nil, errors.New("WSS observation source contains a nil implementation")
	}
	if options.Lifecycle == nil {
		return nil, errors.New("Agent lifecycle gate is required")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if err := options.Identity.Validate(); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(options.Endpoint)
	if err != nil || parsed.Scheme != "wss" || parsed.Host == "" ||
		parsed.Path != "/internal/agents/ws" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("WSS endpoint is invalid")
	}
	return &Client{
		endpoint: options.Endpoint, identity: options.Identity, version: options.Version,
		capabilities: append([]capability.Descriptor(nil), options.Capabilities...),
		executor:     options.Executor, observations: options.Observations,
		lifecycle: options.Lifecycle, onReady: options.OnReady, logger: options.Logger,
		running: make(map[string]*lifecycle.Lease), pending: make(map[string]pendingOperation),
		completed: make(map[string]protocol.ExecutionResult),
	}, nil
}

func isNilObservationSource(source ObservationSource) bool {
	value := reflect.ValueOf(source)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (c *Client) Run(ctx context.Context) error {
	if c.observations == nil {
		return c.runControlLoop(ctx)
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	monitorDone := make(chan error, 1)
	controlDone := make(chan error, 1)
	go func() {
		monitorDone <- c.observations.Run(runContext, c.publishSnapshot, c.publishObservation, c.publishUnchanged)
	}()
	go func() { controlDone <- c.runControlLoop(runContext) }()
	select {
	case err := <-monitorDone:
		cancel()
		<-controlDone
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.logger.Error("IP observation monitor stopped", "code", "ip_monitor_failed")
		if err == nil {
			return errors.New("IP observation monitor stopped unexpectedly")
		}
		return fmt.Errorf("IP observation monitor failed: %w", err)
	case err := <-controlDone:
		cancel()
		<-monitorDone
		return err
	}
}

func (c *Client) runControlLoop(ctx context.Context) error {
	backoff := time.Second
	for ctx.Err() == nil {
		err := c.runSession(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if fatalError := c.executionFailure(); fatalError != nil {
			return fatalError
		}
		delay := backoff + time.Duration(rand.Int64N(max(1, int64(backoff/4))))
		c.logger.Warn("control connection ended", "code", safeConnectionCode(err), "retry_in", delay.String())
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
	return ctx.Err()
}

func (c *Client) runSession(ctx context.Context) error {
	endpoint, _ := url.Parse(c.endpoint)
	query := endpoint.Query()
	query.Set("agent_id", c.identity.AgentID)
	endpoint.RawQuery = query.Encode()
	connection, _, err := websocket.Dial(ctx, endpoint.String(), &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return err
	}
	connection.SetReadLimit(protocol.MaxMessage)
	session := &session{connection: connection}
	defer connection.CloseNow()
	if err := c.authenticate(ctx, session); err != nil {
		return err
	}
	if c.onReady != nil {
		if err := c.onReady(); err != nil {
			return fmt.Errorf("notify service readiness: %w", err)
		}
	}
	c.setActive(session)
	defer func() {
		c.clearActive(session)
		c.releasePending()
	}()
	c.logger.Info("control connection ready")
	for {
		messageType, data, err := connection.Read(ctx)
		if err != nil {
			return err
		}
		if messageType != websocket.MessageText {
			return errors.New("binary control message rejected")
		}
		envelope, err := protocol.Decode(data)
		if err != nil {
			return err
		}
		switch envelope.Type {
		case "operation.offer":
			offer, err := protocol.DecodeOperationOffer(envelope)
			if err != nil {
				return err
			}
			if err := c.acceptOffer(ctx, session, offer); err != nil {
				return err
			}
		case "operation.accepted_ack":
			ack, err := protocol.DecodeBody[protocol.AcceptedAckBody](envelope, "command_id", "accepted")
			if err != nil || !protocol.ValidUUID(ack.CommandID) {
				if err == nil {
					err = errors.New("invalid accepted acknowledgement identifier")
				}
				return err
			}
			c.handleAcceptedAck(ctx, ack)
		case "operation.result_ack":
			ack, err := protocol.DecodeBody[protocol.ResultAckBody](envelope, "command_id", "persisted")
			if err != nil || !protocol.ValidUUID(ack.CommandID) {
				if err == nil {
					err = errors.New("invalid result acknowledgement identifier")
				}
				return err
			}
			if ack.Persisted {
				c.mu.Lock()
				delete(c.completed, ack.CommandID)
				c.mu.Unlock()
			}
		case "ip.snapshot_ack":
			ack, err := protocol.DecodeBody[protocol.IPSnapshotAckBody](envelope, "snapshot_id", "persisted")
			if err != nil || !protocol.ValidUUID(ack.SnapshotID) {
				if err == nil {
					err = errors.New("invalid IP snapshot acknowledgement identifier")
				}
				return err
			}
			if !ack.Persisted || c.observations == nil {
				return errors.New("IP snapshot was not persisted")
			}
			if err := c.observations.AckSnapshot(ack.SnapshotID); err != nil {
				return err
			}
		case "ip.observed_ack":
			ack, err := protocol.DecodeBody[protocol.IPObservationAckBody](envelope, "observation_id", "persisted")
			if err != nil || !protocol.ValidUUID(ack.ObservationID) {
				if err == nil {
					err = errors.New("invalid IP observation acknowledgement identifier")
				}
				return err
			}
			if !ack.Persisted || c.observations == nil {
				return errors.New("IP observation was not persisted")
			}
			if err := c.observations.Ack(ack.ObservationID); err != nil {
				return err
			}
		case "changeip.unchanged_ack":
			ack, err := protocol.DecodeBody[protocol.ChangeIPUnchangedAckBody](envelope, "command_id", "persisted")
			if err != nil || !protocol.ValidUUID(ack.CommandID) {
				if err == nil {
					err = errors.New("invalid ChangeIP acknowledgement identifier")
				}
				return err
			}
			if !ack.Persisted || c.observations == nil {
				return errors.New("ChangeIP unchanged result was not persisted")
			}
			if err := c.observations.AckUnchanged(ack.CommandID); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected control message %q", envelope.Type)
		}
	}
}

func (c *Client) publishUnchanged(result protocol.ChangeIPUnchangedBody) error {
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	if active == nil {
		return errors.New("control connection is not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return active.write(ctx, "changeip.unchanged", result)
}

func (c *Client) publishObservation(observation protocol.IPObservationBody) error {
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	if active == nil {
		return errors.New("control connection is not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return active.write(ctx, "ip.observed", observation)
}

func (c *Client) publishSnapshot(snapshot protocol.IPSnapshotBody) error {
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	if active == nil {
		return errors.New("control connection is not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return active.write(ctx, "ip.snapshot", snapshot)
}

func (c *Client) authenticate(ctx context.Context, session *session) error {
	challengeEnvelope, err := readEnvelope(ctx, session.connection, "auth.challenge")
	if err != nil {
		return err
	}
	challenge, err := protocol.DecodeBody[protocol.AuthChallenge](
		challengeEnvelope, "challenge_id", "agent_id", "nonce", "issued_at", "expires_at",
	)
	if err != nil {
		return err
	}
	if challenge.AgentID != c.identity.AgentID {
		return errors.New("authentication challenge agent mismatch")
	}
	signingText, err := protocol.AuthSigningText(challenge)
	if err != nil {
		return err
	}
	expiresAt, _ := time.Parse(time.RFC3339Nano, challenge.ExpiresAt)
	if time.Now().After(expiresAt) {
		return errors.New("authentication challenge expired")
	}
	signature := ed25519.Sign(c.identity.Ed25519PrivateKey(), signingText)
	if err := session.write(ctx, "auth.response", protocol.AuthResponseBody{
		AgentID: c.identity.AgentID, ChallengeID: challenge.ChallengeID,
		Signature: base64.RawURLEncoding.EncodeToString(signature),
	}); err != nil {
		return err
	}
	authAcceptedEnvelope, err := readEnvelope(ctx, session.connection, "auth.accepted")
	if err != nil {
		return err
	}
	authAccepted, err := protocol.DecodeBody[protocol.AgentIDBody](authAcceptedEnvelope, "agent_id")
	if err != nil || authAccepted.AgentID != c.identity.AgentID {
		return errors.New("authentication acknowledgement agent mismatch")
	}
	if err := session.write(ctx, "agent.hello", protocol.HelloBody{
		AgentVersion: c.version, Capabilities: c.capabilities,
	}); err != nil {
		return err
	}
	helloAcceptedEnvelope, err := readEnvelope(ctx, session.connection, "hello.accepted")
	if err != nil {
		return err
	}
	helloAccepted, err := protocol.DecodeBody[protocol.AgentIDBody](helloAcceptedEnvelope, "agent_id")
	if err != nil || helloAccepted.AgentID != c.identity.AgentID {
		return errors.New("hello acknowledgement agent mismatch")
	}
	return nil
}

func (c *Client) acceptOffer(ctx context.Context, session *session, offer protocol.OperationOffer) error {
	now := time.Now()
	recovery := c.executor.KnownOperation(offer.CommandID, offer.CommandType)
	if !offerWindowAllows(offer, now, recovery) {
		return errors.New("operation offer is invalid")
	}
	c.mu.Lock()
	if result, found := c.completed[offer.CommandID]; found {
		c.mu.Unlock()
		if err := session.write(ctx, "operation.accepted", protocol.CommandIDBody{CommandID: offer.CommandID}); err != nil {
			return err
		}
		return session.write(ctx, "operation.result", resultBody(offer.CommandID, result))
	}
	if _, found := c.running[offer.CommandID]; found {
		c.mu.Unlock()
		return session.write(ctx, "operation.accepted", protocol.CommandIDBody{CommandID: offer.CommandID})
	}
	if _, found := c.pending[offer.CommandID]; found {
		c.mu.Unlock()
		return session.write(ctx, "operation.accepted", protocol.CommandIDBody{CommandID: offer.CommandID})
	}
	lease, acquired := c.lifecycle.TryOperation()
	if !acquired {
		c.mu.Unlock()
		return errors.New("automatic update is in progress")
	}
	c.pending[offer.CommandID] = pendingOperation{offer: offer, lease: lease, recovery: recovery}
	c.mu.Unlock()
	return session.write(ctx, "operation.accepted", protocol.CommandIDBody{CommandID: offer.CommandID})
}

func offerWindowAllows(offer protocol.OperationOffer, now time.Time, recovery bool) bool {
	return offer.ExpiresAt.After(offer.NotBefore) && !now.Before(offer.NotBefore) &&
		(now.Before(offer.ExpiresAt) || recovery)
}

func (c *Client) handleAcceptedAck(ctx context.Context, ack protocol.AcceptedAckBody) {
	c.mu.Lock()
	pending, found := c.pending[ack.CommandID]
	delete(c.pending, ack.CommandID)
	if !found {
		c.mu.Unlock()
		return
	}
	if !ack.Accepted || !offerWindowAllows(pending.offer, time.Now(), pending.recovery) {
		c.mu.Unlock()
		pending.lease.Release()
		return
	}
	if _, running := c.running[ack.CommandID]; running {
		c.mu.Unlock()
		pending.lease.Release()
		return
	}
	c.running[ack.CommandID] = pending.lease
	c.mu.Unlock()
	go c.execute(ctx, pending.offer)
}

func (c *Client) execute(ctx context.Context, offer protocol.OperationOffer) {
	result, executeError := c.executor.Execute(ctx, offer)
	c.mu.Lock()
	lease := c.running[offer.CommandID]
	delete(c.running, offer.CommandID)
	if executeError != nil {
		active := c.active
		if c.fatalErr == nil {
			c.fatalErr = fmt.Errorf("execute accepted command %s: %w", offer.CommandID, executeError)
		}
		c.mu.Unlock()
		lease.Release()
		c.logger.Error("accepted command has no durable terminal result", "code", "operation_state_persist_failed")
		if active != nil {
			active.connection.CloseNow()
		}
		return
	}
	c.completed[offer.CommandID] = result
	active := c.active
	c.mu.Unlock()
	lease.Release()
	if active != nil {
		writeContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := active.write(writeContext, "operation.result", resultBody(offer.CommandID, result)); err != nil {
			c.logger.Warn("operation result awaits reconnect", "command_id", offer.CommandID, "code", result.Code)
			active.connection.CloseNow()
		}
	}
}

func (c *Client) executionFailure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fatalErr
}

func (c *Client) releasePending() {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[string]pendingOperation)
	c.mu.Unlock()
	for _, operation := range pending {
		operation.lease.Release()
	}
}

func resultBody(commandID string, result protocol.ExecutionResult) protocol.OperationResultBody {
	return protocol.OperationResultBody{
		CommandID: commandID, Outcome: result.Outcome, Code: result.Code, Result: result.Result,
	}
}

func (s *session) write(ctx context.Context, messageType string, body any) error {
	data, err := protocol.Encode(messageType, body)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.connection.Write(ctx, websocket.MessageText, data)
}

func readEnvelope(ctx context.Context, connection *websocket.Conn, expectedType string) (protocol.Envelope, error) {
	messageType, data, err := connection.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, err
	}
	if messageType != websocket.MessageText {
		return protocol.Envelope{}, errors.New("binary control message rejected")
	}
	envelope, err := protocol.Decode(data)
	if err != nil {
		return protocol.Envelope{}, err
	}
	if envelope.Type != expectedType {
		return protocol.Envelope{}, fmt.Errorf("expected %s, received %s", expectedType, envelope.Type)
	}
	return envelope, nil
}

func (c *Client) setActive(session *session) {
	c.mu.Lock()
	c.active = session
	c.mu.Unlock()
}

func (c *Client) clearActive(session *session) {
	c.mu.Lock()
	if c.active == session {
		c.active = nil
	}
	c.mu.Unlock()
}

func safeConnectionCode(err error) string {
	if err == nil {
		return "closed"
	}
	status := websocket.CloseStatus(err)
	if status != -1 {
		return fmt.Sprintf("websocket_%d", status)
	}
	return "connection_failed"
}
