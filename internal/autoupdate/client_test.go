package autoupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/bootstrap"
	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

func TestClientWaitUsesIndependentSignatureAndCancels(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentials := identity.Identity{SchemaVersion: identity.SchemaVersion, EnrollmentState: identity.EnrollmentConfirmed,
		AgentID:   "123e4567-e89b-42d3-a456-426614174000",
		PublicKey: base64.RawURLEncoding.EncodeToString(publicKey), PrivateKey: base64.RawURLEncoding.EncodeToString(privateKey)}
	entered := make(chan struct{}, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/agents/maintenance-wait" {
			t.Error("wrong wait path")
			w.WriteHeader(404)
			return
		}
		var request CheckRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		signature, _ := base64.RawURLEncoding.DecodeString(request.Signature)
		text := []byte(strings.Replace(string(SigningText(request)), MaintenanceAuthContext, "akastr-agent-maintenance-wait-v1", 1))
		if !ed25519.Verify(publicKey, text, signature) || ed25519.Verify(publicKey, SigningText(request), signature) {
			t.Error("wait signature must be bound to its own purpose")
		}
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (Client{HTTPClient: server.Client()}).Wait(ctx,
			"wss"+strings.TrimPrefix(server.URL, "https")+"/internal/agents/ws", "v1.0.7", 1, credentials)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("wait did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled wait succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("wait leaked")
	}
}

func TestMaintenanceAcceptsFutureBusinessProtocolAndConfiguration(t *testing.T) {
	manifest := Manifest{Schema: Schema, Status: "update_available",
		Software: SoftwareTarget{Status: "update_available", Version: "v2.0.0", Protocol: "2030-01-01.v99",
			BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v2.0.0/akastr-agent-linux-amd64",
			BinarySHA256: strings.Repeat("b", 64)},
		Configuration: ConfigurationTarget{Status: "update_available", Revision: 2, SchemaVersion: 99, MinimumAgentVersion: "v2.0.0"}}
	if err := manifest.Validate("v1.0.7", 1); err != nil {
		t.Fatal(err)
	}
	manifest.Software.Status = "current"
	if err := manifest.Validate("v2.0.0", 1); err == nil {
		t.Fatal("same binary cannot change its own protocol")
	}
}

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
				Status: "update_available", Revision: 8, SchemaVersion: bootstrap.SchemaVersion, MinimumAgentVersion: "v1.0.7",
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
		Configuration: ConfigurationTarget{Status: "current", Revision: 4, SchemaVersion: bootstrap.SchemaVersion, MinimumAgentVersion: "v1.0.7"},
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

func TestManifestRejectsInvalidBootstrapSchema(t *testing.T) {
	manifest := Manifest{
		Schema: Schema, Status: "current",
		Software: SoftwareTarget{
			Status: "current", Version: "v1.0.7", Protocol: protocol.Version,
			BinaryURL:    "https://github.com/akastrmix/akastr-agent/releases/download/v1.0.7/akastr-agent-linux-amd64",
			BinarySHA256: strings.Repeat("b", 64),
		},
		Configuration: ConfigurationTarget{Status: "current", Revision: 4, SchemaVersion: 0, MinimumAgentVersion: "v1.0.7"},
	}
	if err := manifest.Validate("v1.0.7", 4); err == nil {
		t.Fatal("invalid bootstrap schema accepted")
	}
}

func TestClientReportsOnlySignedBoundedMaintenanceResult(t *testing.T) {
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
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/internal/agents/maintenance-result" {
			t.Errorf("path=%s", request.URL.Path)
			response.WriteHeader(http.StatusNotFound)
			return
		}
		var result maintenanceResultRequest
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&result); err != nil {
			t.Error(err)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		signature, decodeError := base64.RawURLEncoding.DecodeString(result.Signature)
		if decodeError != nil || result.Status != "failed" ||
			result.ErrorCode != "candidate_binary_invalid" ||
			!ed25519.Verify(publicKey, maintenanceResultSigningText(result), signature) {
			t.Errorf("invalid signed result: %+v", result)
		}
		_ = json.NewEncoder(response).Encode(map[string]bool{"persisted": true})
	}))
	defer server.Close()
	endpoint := "wss" + strings.TrimPrefix(server.URL, "https") + "/internal/agents/ws"
	err = (Client{
		HTTPClient: server.Client(),
		Now:        func() time.Time { return time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC) },
		Random:     strings.NewReader(strings.Repeat("r", 32)),
	}).Report(t.Context(), endpoint, "v1.0.6", credentials, MaintenanceResult{
		TargetVersion: "v1.0.7", TargetConfigurationRevision: 8,
		Status: "failed", ErrorCode: "candidate_binary_invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
}
