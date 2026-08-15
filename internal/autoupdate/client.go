package autoupdate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const (
	Schema      = "akastr-agent-update.v1"
	AuthContext = "akastr-agent-update-check-v1"
	maxResponse = 8192
)

var (
	semanticVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	sha256Hex       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type CheckRequest struct {
	AgentID      string `json:"agent_id"`
	AgentVersion string `json:"agent_version"`
	Protocol     string `json:"protocol"`
	Nonce        string `json:"nonce"`
	SentAt       string `json:"sent_at"`
	Signature    string `json:"signature"`
}

type Manifest struct {
	Schema       string `json:"schema"`
	Status       string `json:"status"`
	Version      string `json:"version"`
	Protocol     string `json:"protocol"`
	BinaryURL    string `json:"binary_url"`
	BinarySHA256 string `json:"binary_sha256"`
}

type Client struct {
	HTTPClient *http.Client
	Now        func() time.Time
	Random     io.Reader
}

func (c Client) Check(
	ctx context.Context,
	controlEndpoint string,
	currentVersion string,
	credentials identity.Identity,
) (Manifest, error) {
	if _, err := parseVersion(currentVersion); err != nil {
		return Manifest{}, fmt.Errorf("current Agent version: %w", err)
	}
	if err := credentials.Validate(); err != nil {
		return Manifest{}, err
	}
	endpoint, err := updateEndpoint(controlEndpoint)
	if err != nil {
		return Manifest{}, err
	}
	nonceBytes := make([]byte, 32)
	randomSource := c.Random
	if randomSource == nil {
		randomSource = rand.Reader
	}
	if _, err := io.ReadFull(randomSource, nonceBytes); err != nil {
		return Manifest{}, errors.New("update nonce generation failed")
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	requestBody := CheckRequest{
		AgentID: credentials.AgentID, AgentVersion: currentVersion,
		Protocol: protocol.Version,
		Nonce:    base64.RawURLEncoding.EncodeToString(nonceBytes),
		SentAt:   now().UTC().Format(time.RFC3339Nano),
	}
	requestBody.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(
		credentials.Ed25519PrivateKey(), SigningText(requestBody),
	))
	body, err := json.Marshal(requestBody)
	if err != nil {
		return Manifest{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Manifest{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Akastr-Agent/"+currentVersion)
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return Manifest{}, fmt.Errorf("check Agent update: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponse))
		return Manifest{}, fmt.Errorf("check Agent update: server returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponse+1))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode Agent update manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("Agent update manifest contains trailing JSON")
	}
	if err := manifest.Validate(currentVersion); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func SigningText(request CheckRequest) []byte {
	return []byte(strings.Join([]string{
		AuthContext,
		request.AgentID,
		request.AgentVersion,
		request.Protocol,
		request.Nonce,
		request.SentAt,
	}, "\n"))
}

func (m Manifest) Validate(currentVersion string) error {
	if m.Schema != Schema {
		return errors.New("Agent update manifest schema is unsupported")
	}
	if m.Status != "current" && m.Status != "busy" && m.Status != "update_available" {
		return errors.New("Agent update manifest status is invalid")
	}
	current, err := parseVersion(currentVersion)
	if err != nil {
		return err
	}
	target, err := parseVersion(m.Version)
	if err != nil {
		return fmt.Errorf("Agent update target version: %w", err)
	}
	if m.Protocol != protocol.Version {
		return errors.New("Agent update protocol does not match the running control protocol")
	}
	if !sha256Hex.MatchString(m.BinarySHA256) {
		return errors.New("Agent update binary checksum is invalid")
	}
	expectedURL := "https://github.com/akastrmix/akastr-agent/releases/download/" +
		m.Version + "/akastr-agent-linux-amd64"
	if m.BinaryURL != expectedURL {
		return errors.New("Agent update binary URL is not the approved immutable release asset")
	}
	comparison := compareVersion(target, current)
	if m.Status == "update_available" && comparison <= 0 {
		return errors.New("Agent update manifest attempted a non-forward update")
	}
	if m.Status != "update_available" && comparison > 0 && m.Status == "current" {
		return errors.New("Agent update manifest marked an older Agent as current")
	}
	return nil
}

func updateEndpoint(controlEndpoint string) (string, error) {
	parsed, err := url.Parse(controlEndpoint)
	if err != nil || parsed.Scheme != "wss" || parsed.Host == "" ||
		parsed.Path != "/internal/agents/ws" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("control endpoint cannot derive the Agent update endpoint")
	}
	parsed.Scheme = "https"
	parsed.Path = "/internal/agents/update"
	return parsed.String(), nil
}

type versionParts [3]uint64

func parseVersion(value string) (versionParts, error) {
	match := semanticVersion.FindStringSubmatch(value)
	if match == nil {
		return versionParts{}, errors.New("must be a canonical vMAJOR.MINOR.PATCH version")
	}
	var result versionParts
	for index := range result {
		parsed, err := strconv.ParseUint(match[index+1], 10, 64)
		if err != nil {
			return versionParts{}, errors.New("semantic version component is invalid")
		}
		result[index] = parsed
	}
	return result, nil
}

func compareVersion(left, right versionParts) int {
	for index := range left {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}

func DecodeChecksum(value string) ([]byte, error) {
	if !sha256Hex.MatchString(value) {
		return nil, errors.New("invalid checksum")
	}
	return hex.DecodeString(value)
}
