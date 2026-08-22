package autoupdate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
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

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const (
	Schema                         = "akastr-agent-maintenance.v1"
	ConfigurationSchema            = "akastr-agent-configuration.v1"
	MaintenanceAuthContext         = "akastr-agent-maintenance-check-v1"
	ConfigurationFetchAuthContext  = "akastr-agent-configuration-fetch-v1"
	ConfigurationAcceptAuthContext = "akastr-agent-configuration-accept-v1"
	maxResponse                    = 128 * 1024
)

var (
	semanticVersion   = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	sha256Hex         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	deploymentPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-r[1-9][0-9]*$`)
)

type CheckRequest struct {
	AgentID               string `json:"agent_id"`
	AgentVersion          string `json:"agent_version"`
	ConfigurationRevision int64  `json:"configuration_revision"`
	Protocol              string `json:"protocol"`
	Nonce                 string `json:"nonce"`
	SentAt                string `json:"sent_at"`
	Signature             string `json:"signature"`
}

type SoftwareTarget struct {
	Status       string `json:"status"`
	Version      string `json:"version"`
	Protocol     string `json:"protocol"`
	BinaryURL    string `json:"binary_url"`
	BinarySHA256 string `json:"binary_sha256"`
}

type ConfigurationTarget struct {
	Status              string `json:"status"`
	Revision            int64  `json:"revision"`
	SchemaVersion       int    `json:"schema_version"`
	MinimumAgentVersion string `json:"minimum_agent_version"`
}

type Manifest struct {
	Schema        string              `json:"schema"`
	Status        string              `json:"status"`
	Software      SoftwareTarget      `json:"software"`
	Configuration ConfigurationTarget `json:"configuration"`
}

type Configuration struct {
	Schema                 string          `json:"schema"`
	ConfigurationRevision  int64           `json:"configuration_revision"`
	BootstrapSchemaVersion int             `json:"bootstrap_schema_version"`
	MinimumAgentVersion    string          `json:"minimum_agent_version"`
	Bootstrap              json.RawMessage `json:"bootstrap"`
}

type Client struct {
	HTTPClient *http.Client
	Now        func() time.Time
	Random     io.Reader
}

func (c Client) Check(ctx context.Context, controlEndpoint, currentVersion string, currentRevision int64, credentials identity.Identity) (Manifest, error) {
	if _, err := parseVersion(currentVersion); err != nil || currentRevision < 1 {
		return Manifest{}, errors.New("current Agent maintenance state is invalid")
	}
	request := CheckRequest{
		AgentID: credentials.AgentID, AgentVersion: currentVersion,
		ConfigurationRevision: currentRevision, Protocol: protocol.Version,
	}
	if err := c.sign(credentials, &request.Nonce, &request.SentAt, &request.Signature, func() []byte { return SigningText(request) }); err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := c.post(ctx, controlEndpoint, "/internal/agents/maintenance", currentVersion, request, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("check Agent maintenance: %w", err)
	}
	if err := manifest.Validate(currentVersion, currentRevision); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func SigningText(request CheckRequest) []byte {
	return []byte(strings.Join([]string{
		MaintenanceAuthContext, request.AgentID, request.AgentVersion,
		strconv.FormatInt(request.ConfigurationRevision, 10), request.Protocol,
		request.Nonce, request.SentAt,
	}, "\n"))
}

type configurationFetchRequest struct {
	AgentID               string `json:"agent_id"`
	ConfigurationRevision int64  `json:"configuration_revision"`
	Nonce                 string `json:"nonce"`
	SentAt                string `json:"sent_at"`
	Signature             string `json:"signature"`
}

func (c Client) FetchConfiguration(ctx context.Context, controlEndpoint string, revision int64, credentials identity.Identity, userAgentVersion string) (Configuration, error) {
	request := configurationFetchRequest{AgentID: credentials.AgentID, ConfigurationRevision: revision}
	if err := c.sign(credentials, &request.Nonce, &request.SentAt, &request.Signature, func() []byte { return configurationFetchSigningText(request) }); err != nil {
		return Configuration{}, err
	}
	var configuration Configuration
	if err := c.post(ctx, controlEndpoint, "/internal/agents/configuration", userAgentVersion, request, &configuration); err != nil {
		return Configuration{}, fmt.Errorf("fetch Agent configuration: %w", err)
	}
	if configuration.Schema != ConfigurationSchema || configuration.ConfigurationRevision != revision ||
		configuration.BootstrapSchemaVersion < 1 || !semanticVersion.MatchString(configuration.MinimumAgentVersion) || len(configuration.Bootstrap) == 0 {
		return Configuration{}, errors.New("Agent configuration response is invalid")
	}
	return configuration, nil
}

func configurationFetchSigningText(request configurationFetchRequest) []byte {
	return []byte(strings.Join([]string{
		ConfigurationFetchAuthContext, request.AgentID,
		strconv.FormatInt(request.ConfigurationRevision, 10), request.Nonce, request.SentAt,
	}, "\n"))
}

type configurationAcceptRequest struct {
	AgentID               string                  `json:"agent_id"`
	AgentVersion          string                  `json:"agent_version"`
	ConfigurationRevision int64                   `json:"configuration_revision"`
	Capabilities          []capability.Descriptor `json:"capabilities"`
	CapabilitiesSHA256    string                  `json:"capabilities_sha256"`
	Nonce                 string                  `json:"nonce"`
	SentAt                string                  `json:"sent_at"`
	Signature             string                  `json:"signature"`
}

func (c Client) AcceptConfiguration(ctx context.Context, controlEndpoint, agentVersion string, revision int64, capabilities []capability.Descriptor, credentials identity.Identity) error {
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		return err
	}
	request := configurationAcceptRequest{
		AgentID: credentials.AgentID, AgentVersion: agentVersion,
		ConfigurationRevision: revision, Capabilities: capabilities,
		CapabilitiesSHA256: fmt.Sprintf("%x", sha256.Sum256(encoded)),
	}
	if err := c.sign(credentials, &request.Nonce, &request.SentAt, &request.Signature, func() []byte { return configurationAcceptSigningText(request) }); err != nil {
		return err
	}
	var response struct {
		OK                    bool   `json:"ok"`
		AgentID               string `json:"agent_id"`
		ConfigurationRevision int64  `json:"configuration_revision"`
		Accepted              bool   `json:"accepted"`
	}
	if err := c.post(ctx, controlEndpoint, "/internal/agents/configuration/accept", agentVersion, request, &response); err != nil {
		return fmt.Errorf("accept Agent configuration: %w", err)
	}
	if !response.OK || !response.Accepted || response.AgentID != credentials.AgentID || response.ConfigurationRevision != revision {
		return errors.New("Agent configuration acceptance response is invalid")
	}
	return nil
}

func configurationAcceptSigningText(request configurationAcceptRequest) []byte {
	return []byte(strings.Join([]string{
		ConfigurationAcceptAuthContext, request.AgentID, request.AgentVersion,
		strconv.FormatInt(request.ConfigurationRevision, 10), request.CapabilitiesSHA256,
		request.Nonce, request.SentAt,
	}, "\n"))
}

func (c Client) sign(credentials identity.Identity, nonce, sentAt, signature *string, signingText func() []byte) error {
	if err := credentials.Validate(); err != nil {
		return err
	}
	nonceBytes := make([]byte, 32)
	randomSource := c.Random
	if randomSource == nil {
		randomSource = rand.Reader
	}
	if _, err := io.ReadFull(randomSource, nonceBytes); err != nil {
		return errors.New("maintenance nonce generation failed")
	}
	*nonce = base64.RawURLEncoding.EncodeToString(nonceBytes)
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	*sentAt = now().UTC().Format(time.RFC3339Nano)
	*signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(credentials.Ed25519PrivateKey(), signingText()))
	return nil
}

func (c Client) post(ctx context.Context, controlEndpoint, endpointPath, version string, input, output any) error {
	endpoint, err := maintenanceEndpoint(controlEndpoint, endpointPath)
	if err != nil {
		return err
	}
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Akastr-Agent/"+version)
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	strict := *httpClient
	strict.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := strict.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("server returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponse+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("response contains trailing JSON")
	}
	return nil
}

func (m Manifest) Validate(currentVersion string, currentRevision int64) error {
	if m.Schema != Schema || (m.Status != "current" && m.Status != "busy" && m.Status != "update_available") {
		return errors.New("Agent maintenance manifest is invalid")
	}
	if (m.Software.Status != "current" && m.Software.Status != "update_available") ||
		(m.Configuration.Status != "current" && m.Configuration.Status != "update_available") {
		return errors.New("Agent maintenance target status is invalid")
	}
	current, err := parseVersion(currentVersion)
	if err != nil {
		return err
	}
	target, err := parseVersion(m.Software.Version)
	if err != nil || m.Software.Protocol != protocol.Version || !sha256Hex.MatchString(m.Software.BinarySHA256) {
		return errors.New("Agent software target is invalid")
	}
	expectedURL := "https://github.com/akastrmix/akastr-agent/releases/download/" + m.Software.Version + "/akastr-agent-linux-amd64"
	softwareChanged := compareVersion(target, current) > 0
	if m.Software.BinaryURL != expectedURL || compareVersion(target, current) < 0 || softwareChanged != (m.Software.Status == "update_available") {
		return errors.New("Agent software target is not a forward exact release")
	}
	minimum, err := parseVersion(m.Configuration.MinimumAgentVersion)
	configurationChanged := m.Configuration.Revision > currentRevision
	if err != nil || compareVersion(target, minimum) < 0 || m.Configuration.Revision < currentRevision || m.Configuration.SchemaVersion < 1 ||
		configurationChanged != (m.Configuration.Status == "update_available") {
		return errors.New("Agent configuration target is invalid")
	}
	changed := softwareChanged || configurationChanged
	if m.Status == "busy" {
		if !changed {
			return errors.New("Agent maintenance aggregate status is inconsistent")
		}
	} else if (m.Status == "update_available") != changed {
		return errors.New("Agent maintenance aggregate status is inconsistent")
	}
	return nil
}

func maintenanceEndpoint(controlEndpoint, endpointPath string) (string, error) {
	parsed, err := url.Parse(controlEndpoint)
	if err != nil || parsed.Scheme != "wss" || parsed.Host == "" || parsed.Path != "/internal/agents/ws" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("control endpoint cannot derive an Agent maintenance endpoint")
	}
	parsed.Scheme = "https"
	parsed.Path = endpointPath
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
