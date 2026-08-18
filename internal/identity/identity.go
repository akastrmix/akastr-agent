package identity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/state"
)

const SchemaVersion = 2

const (
	EnrollmentPending   = "pending"
	EnrollmentConfirmed = "confirmed"
)

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type Identity struct {
	SchemaVersion   int    `json:"schema_version"`
	EnrollmentState string `json:"enrollment_state"`
	AgentID         string `json:"agent_id"`
	PublicKey       string `json:"public_key"`
	PrivateKey      string `json:"private_key"`
}

type enrollmentRequest struct {
	MachineToken string                  `json:"machine_token"`
	PublicKey    string                  `json:"public_key"`
	AgentVersion string                  `json:"agent_version"`
	Capabilities []capability.Descriptor `json:"capabilities"`
}

type enrollmentResponse struct {
	OK       bool   `json:"ok"`
	AgentID  string `json:"agent_id"`
	Protocol string `json:"protocol"`
}

type enrollmentErrorResponse struct {
	Error  string          `json:"error"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

var definitiveEnrollmentErrors = map[string]int{
	"agent_enrollment_input_invalid":      http.StatusBadRequest,
	"agent_enrollment_invalid":            http.StatusForbidden,
	"agent_enrollment_public_key_invalid": http.StatusBadRequest,
	"agent_version_invalid":               http.StatusBadRequest,
	"agent_capabilities_invalid":          http.StatusBadRequest,
	"agent_role_capabilities_invalid":     http.StatusConflict,
	"agent_runner_conflict":               http.StatusConflict,
	"agent_node_busy":                     http.StatusConflict,
}

func Load(filePath string) (Identity, error) {
	identity, err := loadStored(filePath)
	if err != nil {
		return Identity{}, err
	}
	if identity.EnrollmentState != EnrollmentConfirmed {
		return Identity{}, errors.New("identity enrollment is not confirmed")
	}
	return identity, nil
}

func loadStored(filePath string) (Identity, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return Identity{}, fmt.Errorf("stat identity: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Identity{}, errors.New("identity must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return Identity{}, errors.New("identity permissions must not grant group or other access")
	}
	var identity Identity
	found, err := state.NewJSONFile(filePath).Load(&identity)
	if err != nil {
		return Identity{}, err
	}
	if !found {
		return Identity{}, errors.New("identity does not exist")
	}
	if err := identity.validateStored(); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

func (i Identity) Validate() error {
	if i.EnrollmentState != EnrollmentConfirmed {
		return errors.New("identity enrollment is not confirmed")
	}
	return i.validateStored()
}

func (i Identity) validateStored() error {
	if i.SchemaVersion != SchemaVersion {
		return fmt.Errorf("identity schema_version must be %d", SchemaVersion)
	}
	if i.EnrollmentState != EnrollmentPending && i.EnrollmentState != EnrollmentConfirmed {
		return errors.New("identity enrollment_state is invalid")
	}
	if !canonicalUUID.MatchString(i.AgentID) {
		return errors.New("identity agent_id is invalid")
	}
	publicKey, err := decodeKey(i.PublicKey, ed25519.PublicKeySize)
	if err != nil {
		return fmt.Errorf("identity public_key: %w", err)
	}
	privateKey, err := decodeKey(i.PrivateKey, ed25519.PrivateKeySize)
	if err != nil {
		return fmt.Errorf("identity private_key: %w", err)
	}
	if !bytes.Equal(privateKey[32:], publicKey) {
		return errors.New("identity public and private keys do not match")
	}
	return nil
}

func (i Identity) Ed25519PrivateKey() ed25519.PrivateKey {
	decoded, _ := base64.RawURLEncoding.DecodeString(i.PrivateKey)
	return ed25519.PrivateKey(decoded)
}

func Enroll(ctx context.Context, options struct {
	Endpoint        string
	TokenFile       string
	IdentityFile    string
	ExpectedAgentID string
	AgentVersion    string
	Capabilities    []capability.Descriptor
	HTTPClient      *http.Client
}) (Identity, error) {
	tokenInfo, err := os.Stat(options.TokenFile)
	if err != nil {
		return Identity{}, fmt.Errorf("stat machine token: %w", err)
	}
	if !tokenInfo.Mode().IsRegular() ||
		(runtime.GOOS != "windows" && tokenInfo.Mode().Perm()&0o077 != 0) {
		return Identity{}, errors.New("machine token must be a root-only regular file")
	}
	tokenBytes, err := os.ReadFile(options.TokenFile)
	if err != nil {
		return Identity{}, fmt.Errorf("read machine token: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if _, err := decodeKey(token, 32); err != nil {
		return Identity{}, errors.New("machine token must be canonical base64url for 32 bytes")
	}
	var identity Identity
	if _, statError := os.Stat(options.IdentityFile); statError == nil {
		identity, err = loadStored(options.IdentityFile)
		if err != nil {
			return Identity{}, err
		}
		if identity.EnrollmentState != EnrollmentPending || identity.AgentID != options.ExpectedAgentID {
			return Identity{}, errors.New("confirmed or mismatched identity already exists")
		}
	} else if !errors.Is(statError, os.ErrNotExist) {
		return Identity{}, fmt.Errorf("stat identity: %w", statError)
	} else {
		publicKey, privateKey, generateError := ed25519.GenerateKey(rand.Reader)
		if generateError != nil {
			return Identity{}, fmt.Errorf("generate identity: %w", generateError)
		}
		identity = Identity{
			SchemaVersion:   SchemaVersion,
			EnrollmentState: EnrollmentPending,
			AgentID:         options.ExpectedAgentID,
			PublicKey:       base64.RawURLEncoding.EncodeToString(publicKey),
			PrivateKey:      base64.RawURLEncoding.EncodeToString(privateKey),
		}
		if err := state.NewJSONFile(options.IdentityFile).Save(identity); err != nil {
			return Identity{}, fmt.Errorf("persist pending identity: %w", err)
		}
	}
	body, err := json.Marshal(enrollmentRequest{
		MachineToken: token,
		PublicKey:    identity.PublicKey,
		AgentVersion: options.AgentVersion,
		Capabilities: options.Capabilities,
	})
	if err != nil {
		return Identity{}, err
	}
	enrollmentURL, err := deriveEnrollmentURL(options.Endpoint)
	if err != nil {
		return Identity{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, enrollmentURL, bytes.NewReader(body))
	if err != nil {
		return Identity{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	strictClient := *client
	strictClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := strictClient.Do(request)
	if err != nil {
		return Identity{}, fmt.Errorf("enroll identity: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if enrollmentRejected(response) {
			if removeError := state.NewJSONFile(options.IdentityFile).Remove(); removeError != nil {
				return Identity{}, fmt.Errorf(
					"enroll identity: server returned HTTP %d and pending identity removal failed: %w",
					response.StatusCode, removeError,
				)
			}
		}
		return Identity{}, fmt.Errorf("enroll identity: server returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4097))
	decoder.DisallowUnknownFields()
	var result enrollmentResponse
	if err := decoder.Decode(&result); err != nil {
		return Identity{}, fmt.Errorf("decode enrollment response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Identity{}, errors.New("enrollment response contains trailing JSON")
	}
	if !result.OK || result.Protocol != protocol.Version || result.AgentID != options.ExpectedAgentID {
		return Identity{}, errors.New("enrollment response identity or protocol mismatch")
	}
	identity.EnrollmentState = EnrollmentConfirmed
	if err := state.NewJSONFile(options.IdentityFile).Save(identity); err != nil {
		return Identity{}, fmt.Errorf("persist confirmed identity: %w", err)
	}
	return identity, nil
}

func enrollmentRejected(response *http.Response) bool {
	if response.StatusCode < 400 || response.StatusCode >= 500 ||
		response.StatusCode == http.StatusRequestTimeout ||
		response.StatusCode == http.StatusTooEarly ||
		response.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return false
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(body) > 4096 {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var result enrollmentErrorResponse
	if err := decoder.Decode(&result); err != nil {
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return false
	}
	if len(result.Detail) > 0 {
		var detail map[string]any
		if err := json.Unmarshal(result.Detail, &detail); err != nil || detail == nil {
			return false
		}
	}
	expectedStatus, ok := definitiveEnrollmentErrors[result.Error]
	return ok && response.StatusCode == expectedStatus
}

func deriveEnrollmentURL(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "wss" || parsed.Host == "" ||
		parsed.Path != "/internal/agents/ws" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("control endpoint cannot derive enrollment URL")
	}
	parsed.Scheme = "https"
	parsed.Path = "/internal/agents/enroll"
	return parsed.String(), nil
}

func decodeKey(value string, length int) ([]byte, error) {
	if value == "" || strings.Contains(value, "=") {
		return nil, errors.New("must use unpadded base64url")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != length || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, errors.New("invalid canonical base64url length")
	}
	return decoded, nil
}
