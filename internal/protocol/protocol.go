package protocol

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/capability"
)

const (
	Version     = "2026-08-16.v2"
	AuthContext = "akastr-agent-auth-v1"
	MaxMessage  = 64 * 1024
)

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type Envelope struct {
	Protocol  string          `json:"protocol"`
	MessageID string          `json:"message_id"`
	Type      string          `json:"type"`
	SentAt    time.Time       `json:"sent_at"`
	Body      json.RawMessage `json:"body"`
}

type AuthChallenge struct {
	ChallengeID string `json:"challenge_id"`
	AgentID     string `json:"agent_id"`
	Nonce       string `json:"nonce"`
	IssuedAt    string `json:"issued_at"`
	ExpiresAt   string `json:"expires_at"`
}

type OperationOffer struct {
	CommandID      string          `json:"command_id"`
	CommandType    string          `json:"command_type"`
	PayloadVersion int             `json:"payload_version"`
	Payload        json.RawMessage `json:"payload"`
	NotBefore      time.Time       `json:"not_before"`
	ExpiresAt      time.Time       `json:"expires_at"`
}

type ExecutionResult struct {
	Outcome string         `json:"outcome"`
	Code    string         `json:"code"`
	Result  map[string]any `json:"result"`
}

type ChangeIPPayload struct {
	ExpectedIPv4 string `json:"expected_ipv4"`
}

type IPQualityPayload struct {
	TargetServerID string `json:"target_server_id"`
	ExpectedIPv4   string `json:"expected_ipv4"`
	ProxyHost      string `json:"proxy_host"`
	ProxyPort      int    `json:"proxy_port"`
	ProxyProfileID string `json:"proxy_profile_id"`
	ScriptVersion  string `json:"script_version"`
}

type IPObservationBody struct {
	ObservationID   string `json:"observation_id"`
	Family          string `json:"family"`
	PreviousAddress string `json:"previous_address"`
	Address         string `json:"address"`
	ObservedAt      string `json:"observed_at"`
}

type IPObservationAckBody struct {
	ObservationID string `json:"observation_id"`
	Persisted     bool   `json:"persisted"`
}

func Encode(messageType string, body any) ([]byte, error) {
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{
		Protocol: Version, MessageID: NewUUID(), Type: messageType,
		SentAt: time.Now().UTC(), Body: bodyJSON,
	})
}

func Decode(data []byte) (Envelope, error) {
	if len(data) < 2 || len(data) > MaxMessage {
		return Envelope{}, errors.New("protocol message size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope Envelope
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, fmt.Errorf("decode protocol envelope: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return Envelope{}, err
	}
	if envelope.Protocol != Version {
		return Envelope{}, errors.New("unsupported protocol version")
	}
	if !canonicalUUID.MatchString(envelope.MessageID) || envelope.SentAt.IsZero() || len(envelope.Body) == 0 {
		return Envelope{}, errors.New("invalid protocol envelope")
	}
	return envelope, nil
}

func DecodeBody[T any](envelope Envelope) (T, error) {
	var result T
	decoder := json.NewDecoder(bytes.NewReader(envelope.Body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("decode %s body: %w", envelope.Type, err)
	}
	if err := requireEOF(decoder); err != nil {
		return result, err
	}
	return result, nil
}

func ValidUUID(value string) bool {
	return canonicalUUID.MatchString(value)
}

func requireEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("protocol JSON contains multiple values")
		}
		return fmt.Errorf("decode protocol trailing data: %w", err)
	}
	return nil
}

func AuthSigningText(challenge AuthChallenge) ([]byte, error) {
	if !canonicalUUID.MatchString(challenge.AgentID) || !canonicalUUID.MatchString(challenge.ChallengeID) {
		return nil, errors.New("invalid authentication challenge identifiers")
	}
	nonce, err := base64.RawURLEncoding.DecodeString(challenge.Nonce)
	if err != nil || len(nonce) != 32 || base64.RawURLEncoding.EncodeToString(nonce) != challenge.Nonce {
		return nil, errors.New("invalid authentication challenge nonce")
	}
	issuedAt, issuedError := time.Parse(time.RFC3339Nano, challenge.IssuedAt)
	expiresAt, expiresError := time.Parse(time.RFC3339Nano, challenge.ExpiresAt)
	if issuedError != nil || expiresError != nil || !expiresAt.After(issuedAt) {
		return nil, errors.New("invalid authentication challenge time")
	}
	return []byte(strings.Join([]string{
		AuthContext,
		challenge.AgentID,
		challenge.ChallengeID,
		challenge.Nonce,
		challenge.IssuedAt,
		challenge.ExpiresAt,
	}, "\n")), nil
}

type HelloBody struct {
	AgentVersion string                  `json:"agent_version"`
	Capabilities []capability.Descriptor `json:"capabilities"`
}

type AuthResponseBody struct {
	AgentID     string `json:"agent_id"`
	ChallengeID string `json:"challenge_id"`
	Signature   string `json:"signature"`
}

type CommandIDBody struct {
	CommandID string `json:"command_id"`
}

type AcceptedAckBody struct {
	CommandID string `json:"command_id"`
	Accepted  bool   `json:"accepted"`
}

type ResultAckBody struct {
	CommandID string `json:"command_id"`
	Persisted bool   `json:"persisted"`
}

type OperationResultBody struct {
	CommandID string         `json:"command_id"`
	Outcome   string         `json:"outcome"`
	Code      string         `json:"code"`
	Result    map[string]any `json:"result"`
}

func NewUUID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		panic("cryptographic random source unavailable")
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(value)
	return hexValue[0:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:32]
}
