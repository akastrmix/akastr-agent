package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/protocol"
	"github.com/akastrmix/akastr-agent/internal/state"
)

func TestEnrollDoesNotFollowRedirectAndRetainsPendingIdentity(t *testing.T) {
	root := t.TempDir()
	tokenBytes := make([]byte, 32)
	for index := range tokenBytes {
		tokenBytes[index] = byte(index + 1)
	}
	tokenFile := filepath.Join(root, "machine-token")
	if err := os.WriteFile(tokenFile, []byte(base64.RawURLEncoding.EncodeToString(tokenBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	var redirectedRequests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirected" {
			redirectedRequests.Add(1)
			response.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(response, request, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	identityFile := filepath.Join(root, "identity.json")
	_, err := Enroll(t.Context(), struct {
		Endpoint              string
		TokenFile             string
		IdentityFile          string
		ExpectedAgentID       string
		AgentVersion          string
		ConfigurationRevision int64
		Capabilities          []capability.Descriptor
		HTTPClient            *http.Client
	}{
		Endpoint:  "wss" + strings.TrimPrefix(server.URL, "https") + "/internal/agents/ws",
		TokenFile: tokenFile, IdentityFile: identityFile,
		ExpectedAgentID: "123e4567-e89b-42d3-a456-426614174000",
		AgentVersion:    "v0.8.0", ConfigurationRevision: 1, HTTPClient: server.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("redirect error = %v, want HTTP 307 rejection", err)
	}
	if !errors.Is(err, ErrEnrollmentOutcomeUncertain) {
		t.Fatalf("redirect error class = %v, want uncertain", err)
	}
	if redirectedRequests.Load() != 0 {
		t.Fatal("enrollment client followed a redirect")
	}
	pending, err := loadStored(identityFile)
	if err != nil || pending.EnrollmentState != EnrollmentPending {
		t.Fatalf("redirect pending identity = %#v, error = %v", pending, err)
	}
}

func TestEnrollmentRejectedRequiresExactBusinessError(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		want        bool
	}{
		{
			name: "invalid token", status: http.StatusForbidden,
			contentType: "application/json; charset=utf-8",
			body:        `{"error":"agent_enrollment_invalid"}`,
			want:        true,
		},
		{
			name: "node busy with detail", status: http.StatusConflict,
			contentType: "application/json",
			body:        `{"error":"agent_node_busy","detail":{"command_id":"test"}}`,
			want:        true,
		},
		{
			name: "redirect", status: http.StatusTemporaryRedirect,
			contentType: "application/json", body: `{"error":"agent_enrollment_invalid"}`,
		},
		{
			name: "request timeout", status: http.StatusRequestTimeout,
			contentType: "application/json", body: `{"error":"agent_enrollment_invalid"}`,
		},
		{
			name: "server error", status: http.StatusServiceUnavailable,
			contentType: "application/json", body: `{"error":"agent_enrollment_invalid"}`,
		},
		{
			name: "proxy page", status: http.StatusForbidden,
			contentType: "text/html", body: `<html>denied</html>`,
		},
		{
			name: "generic JSON error", status: http.StatusForbidden,
			contentType: "application/json", body: `{"error":"bad_request"}`,
		},
		{
			name: "unknown response field", status: http.StatusForbidden,
			contentType: "application/json",
			body:        `{"error":"agent_enrollment_invalid","request_id":"test"}`,
		},
		{
			name: "known error with wrong status", status: http.StatusBadRequest,
			contentType: "application/json", body: `{"error":"agent_enrollment_invalid"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{
				StatusCode: test.status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(test.body)),
			}
			response.Header.Set("Content-Type", test.contentType)
			if got := enrollmentRejected(response); got != test.want {
				t.Fatalf("enrollmentRejected() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestEnrollRemovesPendingIdentityAfterExplicitBusinessRejection(t *testing.T) {
	root := t.TempDir()
	tokenBytes := make([]byte, 32)
	for index := range tokenBytes {
		tokenBytes[index] = byte(index + 1)
	}
	tokenFile := filepath.Join(root, "machine-token")
	if err := os.WriteFile(tokenFile, []byte(base64.RawURLEncoding.EncodeToString(tokenBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json; charset=utf-8")
		response.WriteHeader(http.StatusForbidden)
		_, _ = response.Write([]byte(`{"error":"agent_enrollment_invalid"}`))
	}))
	defer server.Close()
	identityFile := filepath.Join(root, "identity.json")
	_, err := Enroll(t.Context(), struct {
		Endpoint              string
		TokenFile             string
		IdentityFile          string
		ExpectedAgentID       string
		AgentVersion          string
		ConfigurationRevision int64
		Capabilities          []capability.Descriptor
		HTTPClient            *http.Client
	}{
		Endpoint:  "wss" + strings.TrimPrefix(server.URL, "https") + "/internal/agents/ws",
		TokenFile: tokenFile, IdentityFile: identityFile,
		ExpectedAgentID: "123e4567-e89b-42d3-a456-426614174000",
		AgentVersion:    "v1.0.0", ConfigurationRevision: 1, HTTPClient: server.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("Enroll() error = %v, want HTTP 403", err)
	}
	if !errors.Is(err, ErrEnrollmentRejected) {
		t.Fatalf("rejection error class = %v, want rejected", err)
	}
	if _, err := os.Stat(identityFile); !os.IsNotExist(err) {
		t.Fatalf("definitively rejected enrollment identity stat error = %v", err)
	}
}

func TestLoadValidatesPersistedKeyPair(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(t.TempDir(), "identity.json")
	want := Identity{
		SchemaVersion:   SchemaVersion,
		EnrollmentState: EnrollmentConfirmed,
		AgentID:         "123e4567-e89b-42d3-a456-426614174000",
		PublicKey:       base64.RawURLEncoding.EncodeToString(publicKey),
		PrivateKey:      base64.RawURLEncoding.EncodeToString(privateKey),
	}
	if err := state.NewJSONFile(filePath).Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentID != want.AgentID {
		t.Fatalf("AgentID = %q", got.AgentID)
	}

	corrupt := want
	otherPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	corrupt.PublicKey = base64.RawURLEncoding.EncodeToString(otherPublic)
	if err := state.NewJSONFile(filePath).Save(corrupt); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filePath); err == nil {
		t.Fatal("Load accepted mismatched keys")
	}
	_ = os.Remove(filePath)
}

func TestEnrollReusesPendingIdentityAfterAmbiguousFailure(t *testing.T) {
	root := t.TempDir()
	tokenBytes := make([]byte, 32)
	for index := range tokenBytes {
		tokenBytes[index] = byte(index + 1)
	}
	tokenFile := filepath.Join(root, "machine-token")
	if err := os.WriteFile(tokenFile, []byte(base64.RawURLEncoding.EncodeToString(tokenBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	var firstPublicKey string
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var body enrollmentRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode enrollment request: %v", err)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		requestNumber := requests.Add(1)
		if requestNumber == 1 {
			firstPublicKey = body.PublicKey
			response.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if body.PublicKey != firstPublicKey {
			t.Error("retry generated a different enrollment identity")
		}
		if requestNumber == 3 {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusConflict)
			_, _ = response.Write([]byte(`{"error":"agent_node_busy"}`))
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(enrollmentResponse{
			OK: true, AgentID: "123e4567-e89b-42d3-a456-426614174000", Protocol: protocol.Version,
		})
	}))
	defer server.Close()
	identityFile := filepath.Join(root, "identity.json")
	options := struct {
		Endpoint              string
		TokenFile             string
		IdentityFile          string
		ExpectedAgentID       string
		AgentVersion          string
		ConfigurationRevision int64
		Capabilities          []capability.Descriptor
		HTTPClient            *http.Client
	}{
		Endpoint:  "wss" + strings.TrimPrefix(server.URL, "https") + "/internal/agents/ws",
		TokenFile: tokenFile, IdentityFile: identityFile,
		ExpectedAgentID: "123e4567-e89b-42d3-a456-426614174000",
		AgentVersion:    "v1.0.0", ConfigurationRevision: 1, HTTPClient: server.Client(),
	}
	if _, err := Enroll(t.Context(), options); err == nil || !strings.Contains(err.Error(), "HTTP 503") ||
		!errors.Is(err, ErrEnrollmentOutcomeUncertain) {
		t.Fatalf("first Enroll() error = %v", err)
	}
	pending, err := loadStored(identityFile)
	if err != nil || pending.EnrollmentState != EnrollmentPending {
		t.Fatalf("pending identity = %#v, error = %v", pending, err)
	}
	confirmed, err := Enroll(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.EnrollmentState != EnrollmentConfirmed || confirmed.PublicKey != firstPublicKey {
		t.Fatalf("confirmed identity = %#v", confirmed)
	}
	if _, err := Enroll(t.Context(), options); !errors.Is(err, ErrEnrollmentRejected) {
		t.Fatalf("confirmed identity rejection = %v, want rejected", err)
	}
	preserved, err := loadStored(identityFile)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.EnrollmentState != EnrollmentConfirmed || preserved.PublicKey != firstPublicKey {
		t.Fatalf("preserved confirmed identity = %#v", preserved)
	}
}
