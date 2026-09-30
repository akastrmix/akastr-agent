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
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/coder/websocket"
)

// Runtime is the node's enabled modules as seen by the control connection. The
// client owns the session and the operation handshake; everything else goes to
// the runtime.
type Runtime interface {
	// Accepting validates an offer and reports whether it may be accepted now.
	Accepting(protocol.OperationOffer) (bool, error)
	Execute(context.Context, protocol.OperationOffer) (protocol.ExecutionResult, error)
	// Run keeps module reporters running; they publish on the ready session.
	Run(context.Context, module.Publish) error
	ControlReady()
	// Handle processes a module message; false means no module owns its type.
	Handle(protocol.Envelope) (bool, error)
}

type Client struct {
	endpoint              string
	identity              identity.Identity
	version               string
	configurationRevision int64
	capabilities          []capability.Descriptor
	runtime               Runtime
	lifecycle             *lifecycle.Gate
	onReady               func() error
	onSessionEnd          func()
	logger                *slog.Logger
	heartbeatInterval     time.Duration
	heartbeatTimeout      time.Duration

	mu       sync.Mutex
	active   *session
	running  map[string]*lifecycle.Lease
	pending  map[string]pendingOperation
	fatalErr error
	readyAt  time.Time // Owned by the serial control loop, independent of operation execution.
}

type pendingOperation struct {
	offer protocol.OperationOffer
	lease *lifecycle.Lease
}

type session struct {
	connection *websocket.Conn
	writeMu    sync.Mutex
}

type Options struct {
	Endpoint              string
	Identity              identity.Identity
	Version               string
	ConfigurationRevision int64
	Capabilities          []capability.Descriptor
	Runtime               Runtime
	Lifecycle             *lifecycle.Gate
	// OnReady runs after hello.accepted and before any business message.
	OnReady func() error
	// OnSessionEnd runs whenever a connection attempt or session ends.
	OnSessionEnd func()
	Logger       *slog.Logger
}

func New(options Options) (*Client, error) {
	if options.Runtime == nil {
		return nil, errors.New("WSS runtime is required")
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
	if options.ConfigurationRevision < 1 {
		return nil, errors.New("Agent configuration revision is invalid")
	}
	parsed, err := url.Parse(options.Endpoint)
	if err != nil || parsed.Scheme != "wss" || parsed.Host == "" ||
		parsed.Path != "/internal/agents/ws" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("WSS endpoint is invalid")
	}
	return &Client{
		endpoint: options.Endpoint, identity: options.Identity, version: options.Version,
		configurationRevision: options.ConfigurationRevision,
		capabilities:          append([]capability.Descriptor(nil), options.Capabilities...),
		runtime:               options.Runtime,
		lifecycle:             options.Lifecycle, onReady: options.OnReady,
		onSessionEnd: options.OnSessionEnd, logger: options.Logger,
		heartbeatInterval: heartbeatInterval, heartbeatTimeout: heartbeatTimeout,
		running: make(map[string]*lifecycle.Lease), pending: make(map[string]pendingOperation),
	}, nil
}

func (c *Client) Run(ctx context.Context) error {
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	modulesDone := make(chan error, 1)
	controlDone := make(chan error, 1)
	go func() { modulesDone <- c.runtime.Run(runContext, c.publish) }()
	go func() { controlDone <- c.runControlLoop(runContext) }()
	select {
	case err := <-modulesDone:
		cancel()
		<-controlDone
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.logger.Error("Agent module stopped", "code", "module_failed")
		return fmt.Errorf("Agent module failed: %w", err)
	case err := <-controlDone:
		cancel()
		<-modulesDone
		return err
	}
}

func (c *Client) runControlLoop(ctx context.Context) error {
	backoff := time.Second
	for ctx.Err() == nil {
		c.readyAt = time.Time{}
		err := c.runSession(ctx)
		if c.onSessionEnd != nil {
			c.onSessionEnd()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if fatalError := c.executionFailure(); fatalError != nil {
			return fatalError
		}
		backoff = sessionBackoff(backoff, c.readyAt, time.Now(), c.heartbeatInterval)
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

func sessionBackoff(backoff time.Duration, readyAt, now time.Time, stable time.Duration) time.Duration {
	if !readyAt.IsZero() && now.Sub(readyAt) >= stable {
		return time.Second
	}
	return backoff
}

func (c *Client) runSession(ctx context.Context) error {
	return c.runSessionWithTimeout(ctx, connectionSetupTimeout)
}

func (c *Client) runSessionWithTimeout(ctx context.Context, setupTimeout time.Duration) error {
	setupContext, cancelSetup := context.WithTimeout(ctx, setupTimeout)
	defer cancelSetup()
	endpoint, _ := url.Parse(c.endpoint)
	query := endpoint.Query()
	query.Set("agent_id", c.identity.AgentID)
	endpoint.RawQuery = query.Encode()
	connection, _, err := websocket.Dial(setupContext, endpoint.String(), &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return err
	}
	connection.SetReadLimit(protocol.MaxMessage)
	session := &session{connection: connection}
	defer connection.CloseNow()
	if err := c.authenticate(setupContext, session); err != nil {
		return err
	}
	cancelSetup()
	defer session.watchConnection(ctx, c.heartbeatInterval, c.heartbeatTimeout, c.logger)()
	if c.onReady != nil {
		if err := c.onReady(); err != nil {
			fatal := fmt.Errorf("complete service readiness: %w", err)
			c.recordFatal(fatal)
			return fatal
		}
	}
	c.setActive(session)
	c.readyAt = time.Now()
	c.runtime.ControlReady()
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
			if !ack.Persisted {
				return errors.New("operation result was not persisted")
			}
		default:
			handled, err := c.runtime.Handle(envelope)
			if err != nil {
				return err
			}
			if !handled {
				return fmt.Errorf("unexpected control message %q", envelope.Type)
			}
		}
	}
}

func (c *Client) publish(messageType string, body any) error {
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	if active == nil {
		return errors.New("control connection is not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return active.write(ctx, messageType, body)
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
		AgentVersion: c.version, ConfigurationRevision: c.configurationRevision,
		Capabilities: c.capabilities,
	}); err != nil {
		return err
	}
	accepted, err := readEnvelope(ctx, session.connection, "hello.accepted")
	if err != nil {
		return err
	}
	acknowledgement, err := protocol.DecodeBody[protocol.AgentIDBody](accepted, "agent_id")
	if err != nil || acknowledgement.AgentID != c.identity.AgentID {
		return errors.New("hello acknowledgement agent mismatch")
	}
	return nil
}

func (c *Client) acceptOffer(ctx context.Context, session *session, offer protocol.OperationOffer) error {
	now := time.Now()
	if !offerHandshakeAllows(offer, now) {
		return errors.New("operation offer is invalid")
	}
	accepting, err := c.runtime.Accepting(offer)
	if err != nil {
		return err
	}
	if !accepting {
		return nil
	}
	c.mu.Lock()
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
		return nil
	}
	c.pending[offer.CommandID] = pendingOperation{offer: offer, lease: lease}
	c.mu.Unlock()
	return session.write(ctx, "operation.accepted", protocol.CommandIDBody{CommandID: offer.CommandID})
}

func offerHandshakeAllows(offer protocol.OperationOffer, now time.Time) bool {
	return offer.ExpiresAt.After(offer.NotBefore) && !now.Before(offer.NotBefore)
}

func (c *Client) handleAcceptedAck(ctx context.Context, ack protocol.AcceptedAckBody) {
	c.mu.Lock()
	pending, found := c.pending[ack.CommandID]
	delete(c.pending, ack.CommandID)
	if !found {
		c.mu.Unlock()
		return
	}
	if !ack.Accepted {
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
	result, executeError := c.runtime.Execute(ctx, offer)
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

func (c *Client) recordFatal(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fatalErr == nil {
		c.fatalErr = err
	}
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
	envelope, err := readProtocolEnvelope(ctx, connection)
	if err != nil {
		return protocol.Envelope{}, err
	}
	if envelope.Type != expectedType {
		return protocol.Envelope{}, fmt.Errorf("expected %s, received %s", expectedType, envelope.Type)
	}
	return envelope, nil
}

func readProtocolEnvelope(ctx context.Context, connection *websocket.Conn) (protocol.Envelope, error) {
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
