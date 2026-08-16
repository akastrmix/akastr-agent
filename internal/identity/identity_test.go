package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/akastrmix/akastr-agent/internal/capability"
	"github.com/akastrmix/akastr-agent/internal/state"
)

func TestEnrollRejectsHTTPRedirects(t *testing.T) {
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
		Endpoint        string
		TokenFile       string
		IdentityFile    string
		ExpectedAgentID string
		AgentVersion    string
		Capabilities    []capability.Descriptor
		HTTPClient      *http.Client
	}{
		Endpoint:  "wss" + strings.TrimPrefix(server.URL, "https") + "/internal/agents/ws",
		TokenFile: tokenFile, IdentityFile: identityFile,
		ExpectedAgentID: "123e4567-e89b-42d3-a456-426614174000",
		AgentVersion:    "v0.8.0", HTTPClient: server.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("redirect error = %v, want HTTP 307 rejection", err)
	}
	if redirectedRequests.Load() != 0 {
		t.Fatal("enrollment client followed a redirect")
	}
	if _, err := os.Stat(identityFile); !os.IsNotExist(err) {
		t.Fatal("definitively rejected enrollment retained a pending identity")
	}
}

func TestLoadValidatesPersistedKeyPair(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(t.TempDir(), "identity.json")
	want := Identity{
		SchemaVersion: 1,
		AgentID:       "123e4567-e89b-42d3-a456-426614174000",
		PublicKey:     base64.RawURLEncoding.EncodeToString(publicKey),
		PrivateKey:    base64.RawURLEncoding.EncodeToString(privateKey),
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
