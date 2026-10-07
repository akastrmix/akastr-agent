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
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/netpolicy"
)

const (
	Version     = "2026-10-07.v9"
	AuthContext = "akastr-agent-auth-v1"
	MaxMessage  = 1 << 20
)

var (
	canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	stableToken   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	moduleName    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	stateKey      = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
)

// maxStateVersion keeps versions exact in JavaScript numbers on the Cloud side.
const maxStateVersion = 1<<53 - 1

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

type AgentIDBody struct {
	AgentID string `json:"agent_id"`
}

// OperationOffer carries a payload that the module owning CommandType validates.
type OperationOffer struct {
	CommandID      string
	CommandType    string
	PayloadVersion int
	Payload        json.RawMessage
	NotBefore      time.Time
	ExpiresAt      time.Time
}

type operationOfferBody struct {
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
	if !canonicalUUID.MatchString(envelope.MessageID) || envelope.Type == "" ||
		envelope.SentAt.IsZero() || len(envelope.Body) == 0 {
		return Envelope{}, errors.New("invalid protocol envelope")
	}
	return envelope, nil
}

func DecodeBody[T any](envelope Envelope, fields ...string) (T, error) {
	return decodeRequiredJSON[T](envelope.Body, envelope.Type, fields...)
}

func DecodeOperationOffer(envelope Envelope) (OperationOffer, error) {
	body, err := DecodeBody[operationOfferBody](
		envelope,
		"command_id", "command_type", "payload_version", "payload", "not_before", "expires_at",
	)
	if err != nil || !ValidUUID(body.CommandID) || body.PayloadVersion != 1 ||
		body.NotBefore.IsZero() || body.ExpiresAt.IsZero() ||
		!body.ExpiresAt.After(body.NotBefore) || len(body.Payload) == 0 {
		return OperationOffer{}, errors.New("operation offer is invalid")
	}
	if !stableToken.MatchString(body.CommandType) {
		return OperationOffer{}, errors.New("operation type is invalid")
	}
	return OperationOffer{
		CommandID: body.CommandID, CommandType: body.CommandType, PayloadVersion: body.PayloadVersion,
		Payload: body.Payload, NotBefore: body.NotBefore, ExpiresAt: body.ExpiresAt,
	}, nil
}

// DecodeKnown decodes a JSON object that may omit fields but not add unknown ones.
func DecodeKnown[T any](data []byte, description string) (T, error) {
	return decodeJSONBody[T](data, description)
}

// DecodeStrict decodes a JSON object that must contain exactly fields.
func DecodeStrict[T any](data []byte, description string, fields ...string) (T, error) {
	return decodeRequiredJSON[T](data, description, fields...)
}

func decodeRequiredJSON[T any](data []byte, description string, fields ...string) (T, error) {
	var result T
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil || len(object) != len(fields) {
		return result, fmt.Errorf("decode %s body: required fields are missing", description)
	}
	for _, field := range fields {
		if _, found := object[field]; !found {
			return result, fmt.Errorf("decode %s body: required field %s is missing", description, field)
		}
	}
	return decodeJSONBody[T](data, description)
}

func decodeJSONBody[T any](data []byte, description string) (T, error) {
	var result T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("decode %s body: %w", description, err)
	}
	if err := requireEOF(decoder); err != nil {
		return result, err
	}
	return result, nil
}

// PublicIPv4 reports whether value is a canonical public IPv4 address.
func PublicIPv4(value string) bool {
	address, err := netip.ParseAddr(value)
	return err == nil && address.String() == value && netpolicy.IsPublicIPv4(address)
}

// StableCode reports whether value is a stable lower-case result code.
func StableCode(value string) bool {
	return stableToken.MatchString(value)
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
	AgentVersion          string `json:"agent_version"`
	ConfigurationRevision int64  `json:"configuration_revision"`
}

// ReportBody carries a fact a module reports on its own; Cloud acknowledges it
// by report_id once it is stored, and the module resends it until then.
type ReportBody struct {
	ReportID string `json:"report_id"`
	Kind     string `json:"kind"`
	Data     any    `json:"data"`
}

type ReportAckBody struct {
	ReportID string `json:"report_id"`
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

// StatePut replaces the whole target of one key of a module.
type StatePut struct {
	Module  string          `json:"module"`
	Key     string          `json:"key"`
	Version int64           `json:"version"`
	State   json.RawMessage `json:"state"`
}

// StateKeys names every key a module currently has; keys missing from it are retired.
type StateKeys struct {
	Module string   `json:"module"`
	Keys   []string `json:"keys"`
}

// StateStatus tells Cloud whether the node is in the target of a key version.
type StateStatus struct {
	Module    string `json:"module"`
	Key       string `json:"key"`
	Version   int64  `json:"version"`
	ErrorCode string `json:"error_code"`
}

func DecodeStatePut(envelope Envelope) (StatePut, error) {
	body, err := DecodeBody[StatePut](envelope, "module", "key", "version", "state")
	if err != nil || !moduleName.MatchString(body.Module) || !stateKey.MatchString(body.Key) ||
		body.Version < 1 || body.Version > maxStateVersion || !jsonObject(body.State) {
		return StatePut{}, errors.New("state put is invalid")
	}
	return body, nil
}

func DecodeStateKeys(envelope Envelope) (StateKeys, error) {
	body, err := DecodeBody[StateKeys](envelope, "module", "keys")
	if err != nil || !moduleName.MatchString(body.Module) || body.Keys == nil {
		return StateKeys{}, errors.New("state keys are invalid")
	}
	seen := make(map[string]bool, len(body.Keys))
	for _, key := range body.Keys {
		if !stateKey.MatchString(key) || seen[key] {
			return StateKeys{}, errors.New("state keys are invalid")
		}
		seen[key] = true
	}
	return body, nil
}

func jsonObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 1 && trimmed[0] == '{' && json.Valid(trimmed)
}
