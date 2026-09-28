// Package identity holds the node's Ed25519 key. Every installation generates a
// fresh key and registers it with the machine token; the key authenticates all
// later WSS sessions and update checks.
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
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/state"
)

const SchemaVersion = 3

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type Identity struct {
	SchemaVersion int    `json:"schema_version"`
	AgentID       string `json:"agent_id"`
	PublicKey     string `json:"public_key"`
	PrivateKey    string `json:"private_key"`
}

func Generate(agentID string) (Identity, error) {
	if !canonicalUUID.MatchString(agentID) {
		return Identity{}, errors.New("identity agent_id is invalid")
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, fmt.Errorf("generate identity: %w", err)
	}
	return Identity{
		SchemaVersion: SchemaVersion, AgentID: agentID,
		PublicKey:  base64.RawURLEncoding.EncodeToString(publicKey),
		PrivateKey: base64.RawURLEncoding.EncodeToString(privateKey),
	}, nil
}

func Load(filePath string) (Identity, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return Identity{}, fmt.Errorf("stat identity: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return Identity{}, errors.New("identity must be a root-only regular file")
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

// ReadAgentID reads only the owner of an existing identity file, whatever its
// schema, so a reinstall can refuse to take over another node's machine.
func ReadAgentID(filePath string) (string, bool, error) {
	raw, err := os.ReadFile(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var owner struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(raw, &owner); err != nil || !canonicalUUID.MatchString(owner.AgentID) {
		return "", true, errors.New("existing identity does not name a valid node")
	}
	return owner.AgentID, true, nil
}

func (i Identity) Save(filePath string) error {
	if err := i.Validate(); err != nil {
		return err
	}
	return state.NewJSONFile(filePath).Save(i)
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

type Enrollment struct {
	ControlEndpoint       string
	MachineToken          string
	AgentVersion          string
	ConfigurationRevision int64
	Capabilities          []capability.Descriptor
	HTTPClient            *http.Client
}

// Enroll registers the identity's public key. Cloud replaces any earlier key of
// the same node, so a failed or repeated install is repaired by rerunning it.
func (i Identity) Enroll(ctx context.Context, enrollment Enrollment) error {
	endpoint, err := url.Parse(enrollment.ControlEndpoint)
	if err != nil || endpoint.Scheme != "wss" || endpoint.Host == "" || endpoint.Path != "/internal/agents/ws" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("control endpoint cannot derive enrollment URL")
	}
	endpoint.Scheme, endpoint.Path = "https", "/internal/agents/enroll"
	body, err := json.Marshal(struct {
		MachineToken          string                  `json:"machine_token"`
		PublicKey             string                  `json:"public_key"`
		AgentVersion          string                  `json:"agent_version"`
		ConfigurationRevision int64                   `json:"configuration_revision"`
		Capabilities          []capability.Descriptor `json:"capabilities"`
	}{
		MachineToken: enrollment.MachineToken, PublicKey: i.PublicKey,
		AgentVersion: enrollment.AgentVersion, ConfigurationRevision: enrollment.ConfigurationRevision,
		Capabilities: enrollment.Capabilities,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	client := enrollment.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	strict := *client
	strict.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := strict.Do(request)
	if err != nil {
		return fmt.Errorf("enroll identity: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil {
		return fmt.Errorf("read enrollment response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &failure) == nil && regexp.MustCompile(`^[a-z0-9_]{1,64}$`).MatchString(failure.Error) {
			return fmt.Errorf("enrollment rejected: HTTP %d %s", response.StatusCode, failure.Error)
		}
		return fmt.Errorf("enrollment rejected: HTTP %d", response.StatusCode)
	}
	var result struct {
		OK       bool   `json:"ok"`
		AgentID  string `json:"agent_id"`
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("decode enrollment response: %w", err)
	}
	if !result.OK || result.AgentID != i.AgentID || result.Protocol != protocol.Version {
		return errors.New("enrollment response identity or protocol mismatch")
	}
	return nil
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
