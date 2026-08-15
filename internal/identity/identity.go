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
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/state"
)

const SchemaVersion = 1

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type Identity struct {
	SchemaVersion int    `json:"schema_version"`
	AgentID       string `json:"agent_id"`
	PublicKey     string `json:"public_key"`
	PrivateKey    string `json:"private_key"`
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

func Load(filePath string) (Identity, error) {
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
	if err := identity.Validate(); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

func (i Identity) Validate() error {
	if i.SchemaVersion != SchemaVersion {
		return fmt.Errorf("identity schema_version must be %d", SchemaVersion)
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
	if _, err := os.Stat(options.IdentityFile); err == nil {
		return Identity{}, errors.New("identity already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Identity{}, fmt.Errorf("stat identity: %w", err)
	}
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
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, fmt.Errorf("generate identity: %w", err)
	}
	body, err := json.Marshal(enrollmentRequest{
		MachineToken: token,
		PublicKey:    base64.RawURLEncoding.EncodeToString(publicKey),
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
	response, err := client.Do(request)
	if err != nil {
		return Identity{}, fmt.Errorf("enroll identity: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
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
	if !result.OK || result.Protocol != "2026-08-16.v3" || result.AgentID != options.ExpectedAgentID {
		return Identity{}, errors.New("enrollment response identity or protocol mismatch")
	}
	identity := Identity{
		SchemaVersion: SchemaVersion,
		AgentID:       result.AgentID,
		PublicKey:     base64.RawURLEncoding.EncodeToString(publicKey),
		PrivateKey:    base64.RawURLEncoding.EncodeToString(privateKey),
	}
	if err := state.NewJSONFile(options.IdentityFile).Save(identity); err != nil {
		return Identity{}, fmt.Errorf("persist identity: %w", err)
	}
	return identity, nil
}

func deriveEnrollmentURL(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "wss" || parsed.Host == "" || !strings.HasSuffix(parsed.Path, "/ws") {
		return "", errors.New("control endpoint cannot derive enrollment URL")
	}
	parsed.Scheme = "https"
	parsed.Path = strings.TrimSuffix(parsed.Path, "/ws") + "/enroll"
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
