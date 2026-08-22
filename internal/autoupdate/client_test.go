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

func TestClientSignsRevisionAwareMaintenanceCheck(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentials := identity.Identity{
		SchemaVersion: identity.SchemaVersion, EnrollmentState: identity.EnrollmentConfirmed,
		AgentID:    "123e4567-e89b-42d3-a456-426614174000",
		PublicKey:  base64.RawURLEncoding.EncodeToString(publicKey),
		PrivateKey: base64.RawURLEncoding.EncodeToString(privateKey),
	}
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/internal/agents/maintenance" {
			t.Errorf("path=%s", request.URL.Path)
			response.WriteHeader(404)
			return
		}
		var check CheckRequest
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&check); err != nil {
			t.Error(err)
			response.WriteHeader(400)
			return
		}
		signature, decodeError := base64.RawURLEncoding.DecodeString(check.Signature)
		if decodeError != nil || check.ConfigurationRevision != 7 || !ed25519.Verify(publicKey, SigningText(check), signature) {
			t.Errorf("invalid signed check: %+v", check)
		}
		_ = json.NewEncoder(response).Encode(Manifest{
			Schema: Schema, Status: "update_available",
			Software: SoftwareTarget{
				Status: "update_available", Version: "v1.0.7", Protocol: protocol.Version,
				BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v1.0.7/akastr-agent-linux-amd64",
				BinarySHA256: strings.Repeat("a", 64),
			},
			Configuration: ConfigurationTarget{
				Status: "update_available", Revision: 8, SchemaVersion: 3, MinimumAgentVersion: "v1.0.7",
			},
		})
	}))
	defer server.Close()
	endpoint := "wss" + strings.TrimPrefix(server.URL, "https") + "/internal/agents/ws"
	manifest, err := (Client{
		HTTPClient: server.Client(), Now: func() time.Time { return now },
		Random: strings.NewReader(strings.Repeat("n", 32)),
	}).Check(t.Context(), endpoint, "v1.0.6", 7, credentials)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Software.Version != "v1.0.7" || manifest.Configuration.Revision != 8 {
		t.Fatalf("manifest=%+v", manifest)
	}
}

func TestManifestRejectsDowngradeAndInconsistentConfiguration(t *testing.T) {
	manifest := Manifest{
		Schema: Schema, Status: "update_available",
		Software: SoftwareTarget{
			Status: "update_available", Version: "v1.0.7", Protocol: protocol.Version,
			BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v1.0.7/akastr-agent-linux-amd64",
			BinarySHA256: strings.Repeat("b", 64),
		},
		Configuration: ConfigurationTarget{Status: "current", Revision: 4, SchemaVersion: 3, MinimumAgentVersion: "v1.0.7"},
	}
	if err := manifest.Validate("v1.0.6", 4); err != nil {
		t.Fatal(err)
	}
	manifest.Software.Version = "v1.0.5"
	manifest.Software.BinaryURL = "https://github.com/akastrmix/akastr-agent/releases/download/v1.0.5/akastr-agent-linux-amd64"
	if err := manifest.Validate("v1.0.6", 4); err == nil {
		t.Fatal("downgrade accepted")
	}
}
