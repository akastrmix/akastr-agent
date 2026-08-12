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
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/coder/websocket"
)

type Executor interface {
	Execute(context.Context, protocol.OperationOffer) protocol.ExecutionResult
}

type ObservationSource interface {
	Run(context.Context, func(protocol.IPObservationBody) error) error
	Ack(string) error
}

type Client struct {
	endpoint     string
	identity     identity.Identity
	version      string
	capabilities []capability.Descriptor
	executor     Executor
	observations ObservationSource
	logger       *slog.Logger

	mu        sync.Mutex
	active    *session
	running   map[string]struct{}
	pending   map[string]protocol.OperationOffer
	completed map[string]protocol.ExecutionResult
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
	Logger       *slog.Logger
}) (*Client, error) {
	if options.Executor == nil {
		return nil, errors.New("WSS executor is required")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if err := options.Identity.Validate(); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(options.Endpoint)
	if err != nil || parsed.Scheme != "wss" || parsed.Host == "" || parsed.RawQuery != "" {
		return nil, errors.New("WSS endpoint is invalid")
	}
	return &Client{
		endpoint: options.Endpoint, identity: options.Identity, version: options.Version,
		capabilities: append([]capability.Descriptor(nil), options.Capabilities...),
		executor:     options.Executor, observations: options.Observations, logger: options.Logger,
		running: make(map[string]struct{}), pending: make(map[string]protocol.OperationOffer),
		completed: make(map[string]protocol.ExecutionResult),
	}, nil
}

func (c *Client) Run(ctx context.Context) error {
	if c.observations != nil {
		go func() {
			if err := c.observations.Run(ctx, c.publishObservation); err != nil && ctx.Err() == nil {
				c.logger.Error("IP observation monitor stopped", "code", "ip_monitor_failed")
			}
		}()
	}
	backoff := time.Second
	for ctx.Err() == nil {
		err := c.runSession(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
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
	c.setActive(session)
	defer c.clearActive(session)
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
			offer, err := protocol.DecodeBody[protocol.OperationOffer](envelope)
			if err != nil {
				return err
			}
			if err := c.acceptOffer(ctx, session, offer); err != nil {
				return err
			}
		case "operation.accepted_ack":
			ack, err := protocol.DecodeBody[protocol.AcceptedAckBody](envelope)
			if err != nil {
				return err
			}
			c.handleAcceptedAck(ctx, ack)
		case "operation.result_ack":
			ack, err := protocol.DecodeBody[protocol.ResultAckBody](envelope)
			if err != nil {
				return err
			}
			if ack.Persisted {
				c.mu.Lock()
				delete(c.completed, ack.CommandID)
				c.mu.Unlock()
			}
		case "ip.observed_ack":
			ack, err := protocol.DecodeBody[protocol.IPObservationAckBody](envelope)
			if err != nil {
				return err
			}
			if !ack.Persisted || c.observations == nil {
				return errors.New("IP observation was not persisted")
			}
			if err := c.observations.Ack(ack.ObservationID); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected control message %q", envelope.Type)
		}
	}
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

func (c *Client) authenticate(ctx context.Context, session *session) error {
	challengeEnvelope, err := readEnvelope(ctx, session.connection, "auth.challenge")
	if err != nil {
		return err
	}
	challenge, err := protocol.DecodeBody[protocol.AuthChallenge](challengeEnvelope)
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
	if _, err := readEnvelope(ctx, session.connection, "auth.accepted"); err != nil {
		return err
	}
	if err := session.write(ctx, "agent.hello", protocol.HelloBody{
		AgentVersion: c.version, Capabilities: c.capabilities,
	}); err != nil {
		return err
	}
	_, err = readEnvelope(ctx, session.connection, "hello.accepted")
	return err
}

func (c *Client) acceptOffer(ctx context.Context, session *session, offer protocol.OperationOffer) error {
	if !protocol.ValidUUID(offer.CommandID) || offer.PayloadVersion != 1 ||
		!offer.ExpiresAt.After(offer.NotBefore) || len(offer.Payload) == 0 {
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
	c.pending[offer.CommandID] = offer
	c.mu.Unlock()
	return session.write(ctx, "operation.accepted", protocol.CommandIDBody{CommandID: offer.CommandID})
}

func (c *Client) handleAcceptedAck(ctx context.Context, ack protocol.AcceptedAckBody) {
	c.mu.Lock()
	offer, found := c.pending[ack.CommandID]
	delete(c.pending, ack.CommandID)
	if !ack.Accepted || !found || time.Now().After(offer.ExpiresAt) {
		c.mu.Unlock()
		return
	}
	if _, running := c.running[ack.CommandID]; running {
		c.mu.Unlock()
		return
	}
	c.running[ack.CommandID] = struct{}{}
	c.mu.Unlock()
	go c.execute(ctx, offer)
}

func (c *Client) execute(ctx context.Context, offer protocol.OperationOffer) {
	result := c.executor.Execute(ctx, offer)
	c.mu.Lock()
	delete(c.running, offer.CommandID)
	c.completed[offer.CommandID] = result
	active := c.active
	c.mu.Unlock()
	if active != nil {
		writeContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := active.write(writeContext, "operation.result", resultBody(offer.CommandID, result)); err != nil {
			c.logger.Warn("operation result awaits reconnect", "command_id", offer.CommandID, "code", result.Code)
		}
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
