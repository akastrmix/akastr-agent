package autoupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

func TestClientAuthenticatesAndValidatesApprovedManifest(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentials := identity.Identity{
		SchemaVersion:   identity.SchemaVersion,
		EnrollmentState: identity.EnrollmentConfirmed,
		AgentID:         "123e4567-e89b-42d3-a456-426614174000",
		PublicKey:       base64.RawURLEncoding.EncodeToString(publicKey),
		PrivateKey:      base64.RawURLEncoding.EncodeToString(privateKey),
	}
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/internal/agents/update" {
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			response.WriteHeader(http.StatusNotFound)
			return
		}
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		var check CheckRequest
		if err := decoder.Decode(&check); err != nil {
			t.Errorf("decode check: %v", err)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		if check.AgentID != credentials.AgentID || check.AgentVersion != "v0.7.0" ||
			check.Protocol != protocol.Version || check.SentAt != now.Format(time.RFC3339Nano) {
			t.Errorf("unexpected signed check: %+v", check)
		}
		signature, err := base64.RawURLEncoding.DecodeString(check.Signature)
		if err != nil || !ed25519.Verify(publicKey, SigningText(check), signature) {
			t.Error("update check signature was invalid")
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(Manifest{
			Schema: Schema, Status: "update_available", Version: "v0.7.1",
			Protocol:     protocol.Version,
			BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v0.7.1/akastr-agent-linux-amd64",
			BinarySHA256: strings.Repeat("a", 64),
		})
	}))
	defer server.Close()
	controlEndpoint := "wss" + strings.TrimPrefix(server.URL, "https") + "/internal/agents/ws"
	manifest, err := (Client{
		HTTPClient: server.Client(), Now: func() time.Time { return now },
		Random: strings.NewReader(strings.Repeat("n", 32)),
	}).Check(t.Context(), controlEndpoint, "v0.7.0", credentials)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "update_available" || manifest.Version != "v0.7.1" {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
}

func TestManifestRejectsDowngradeAndUnapprovedAsset(t *testing.T) {
	base := Manifest{
		Schema: Schema, Status: "update_available", Version: "v0.7.1",
		Protocol:     protocol.Version,
		BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v0.7.1/akastr-agent-linux-amd64",
		BinarySHA256: strings.Repeat("b", 64),
	}
	if err := base.Validate("v0.7.0"); err != nil {
		t.Fatal(err)
	}
	downgrade := base
	downgrade.Version = "v0.6.9"
	downgrade.BinaryURL = "https://github.com/akastrmix/akastr-agent/releases/download/v0.6.9/akastr-agent-linux-amd64"
	if err := downgrade.Validate("v0.7.0"); err == nil {
		t.Fatal("downgrade was accepted")
	}
	malicious := base
	malicious.BinaryURL = "https://example.com/agent"
	if err := malicious.Validate("v0.7.0"); err == nil {
		t.Fatal("unapproved binary URL was accepted")
	}
}
