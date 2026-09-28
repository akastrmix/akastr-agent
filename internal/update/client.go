// Package update keeps a node on the software and configuration that
// AkastrCloud approves. One signed HTTPS check returns the whole target; the
// candidate is written to the inactive slot and started in place of the running
// process, and it activates itself only after Cloud accepts its hello.
package update

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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/identity"
)

const (
	CheckSchema  = "akastr-agent-maintenance.v2"
	CheckContext = "akastr-agent-maintenance-v2"
	maxResponse  = 256 * 1024
)

var (
	semanticVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	sha256Hex       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	stableCode      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

type CheckRequest struct {
	AgentID               string `json:"agent_id"`
	AgentVersion          string `json:"agent_version"`
	ConfigurationRevision int64  `json:"configuration_revision"`
	ErrorCode             string `json:"error_code"`
	Nonce                 string `json:"nonce"`
	SentAt                string `json:"sent_at"`
	Signature             string `json:"signature"`
}

func SigningText(request CheckRequest) []byte {
	return []byte(strings.Join([]string{
		CheckContext, request.AgentID, request.AgentVersion,
		strconv.FormatInt(request.ConfigurationRevision, 10), request.ErrorCode,
		request.Nonce, request.SentAt,
	}, "\n"))
}

// Target is Cloud's answer: the approved software and configuration for this
// node. Configuration is present only when an update changes the revision.
type Target struct {
	Schema                string          `json:"schema"`
	Status                string          `json:"status"`
	Version               string          `json:"version"`
	BinaryURL             string          `json:"binary_url"`
	BinarySHA256          string          `json:"binary_sha256"`
	ConfigurationRevision int64           `json:"configuration_revision"`
	Configuration         json.RawMessage `json:"configuration"`
}

func (t Target) Name() string {
	return t.Version + "-r" + strconv.FormatInt(t.ConfigurationRevision, 10)
}

func (t Target) hasConfiguration() bool {
	return len(t.Configuration) != 0 && string(t.Configuration) != "null"
}

func (t Target) validate(currentVersion string, currentRevision int64) error {
	if t.Schema != CheckSchema || (t.Status != "current" && t.Status != "busy" && t.Status != "update_available") {
		return errors.New("update target status is invalid")
	}
	current, err := parseVersion(currentVersion)
	if err != nil {
		return err
	}
	target, err := parseVersion(t.Version)
	if err != nil || !sha256Hex.MatchString(t.BinarySHA256) ||
		t.BinaryURL != "https://github.com/akastrmix/akastr-agent/releases/download/"+t.Version+"/akastr-agent-linux-amd64" {
		return errors.New("update target software is invalid")
	}
	if compareVersion(target, current) < 0 || t.ConfigurationRevision < currentRevision {
		return errors.New("update target moves backwards")
	}
	changed := t.Version != currentVersion || t.ConfigurationRevision != currentRevision
	if (t.Status == "current") == changed {
		return errors.New("update target status does not match its versions")
	}
	withConfiguration := t.Status == "update_available" && t.ConfigurationRevision != currentRevision
	if t.hasConfiguration() != withConfiguration {
		return errors.New("update target configuration presence is invalid")
	}
	return nil
}

type Client struct {
	HTTPClient *http.Client
	Now        func() time.Time
}

// Check reports the last update failure, if any, and returns the current target.
func (c Client) Check(ctx context.Context, controlEndpoint string, credentials identity.Identity, version string, revision int64, errorCode string) (Target, error) {
	if errorCode != "" && !stableCode.MatchString(errorCode) {
		return Target{}, errors.New("update error code is invalid")
	}
	endpoint, err := url.Parse(controlEndpoint)
	if err != nil || endpoint.Scheme != "wss" || endpoint.Host == "" || endpoint.Path != "/internal/agents/ws" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return Target{}, errors.New("control endpoint cannot derive the update endpoint")
	}
	endpoint.Scheme, endpoint.Path = "https", "/internal/agents/maintenance"
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return Target{}, errors.New("update nonce generation failed")
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	request := CheckRequest{
		AgentID: credentials.AgentID, AgentVersion: version, ConfigurationRevision: revision,
		ErrorCode: errorCode, Nonce: base64.RawURLEncoding.EncodeToString(nonce),
		SentAt: now().UTC().Format(time.RFC3339Nano),
	}
	request.Signature = base64.RawURLEncoding.EncodeToString(
		ed25519.Sign(credentials.Ed25519PrivateKey(), SigningText(request)),
	)
	body, err := json.Marshal(request)
	if err != nil {
		return Target{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Target{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("User-Agent", "Akastr-Agent/"+version)
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	strict := *client
	strict.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := strict.Do(httpRequest)
	if err != nil {
		return Target{}, fmt.Errorf("check for updates: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return Target{}, fmt.Errorf("check for updates: server returned HTTP %d", response.StatusCode)
	}
	return decodeTarget(io.LimitReader(response.Body, maxResponse+1), version, revision)
}

func decodeTarget(body io.Reader, version string, revision int64) (Target, error) {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var target Target
	if err := decoder.Decode(&target); err != nil {
		return Target{}, fmt.Errorf("decode update target: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Target{}, errors.New("update target contains trailing JSON")
	}
	if err := target.validate(version, revision); err != nil {
		return Target{}, err
	}
	return target, nil
}

type versionParts [3]uint64

func parseVersion(value string) (versionParts, error) {
	match := semanticVersion.FindStringSubmatch(value)
	if match == nil {
		return versionParts{}, errors.New("version must be a canonical vMAJOR.MINOR.PATCH")
	}
	var result versionParts
	for index := range result {
		parsed, err := strconv.ParseUint(match[index+1], 10, 64)
		if err != nil {
			return versionParts{}, errors.New("version component is invalid")
		}
		result[index] = parsed
	}
	return result, nil
}

func compareVersion(left, right versionParts) int {
	for index := range left {
		if left[index] != right[index] {
			if left[index] < right[index] {
				return -1
			}
			return 1
		}
	}
	return 0
}
