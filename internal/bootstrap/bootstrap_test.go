package bootstrap

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const testAgentID = "11111111-1111-4111-8111-111111111111"

func testToken(t *testing.T, directory string) (string, []byte, string) {
	t.Helper()
	raw := make([]byte, 32)
	for index := range raw {
		raw[index] = byte(index + 1)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	filePath := filepath.Join(directory, "token")
	if err := os.WriteFile(filePath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return token, raw, filePath
}

func TestFetchRejectsHTTPRedirects(t *testing.T) {
	root := t.TempDir()
	_, _, tokenFile := testToken(t, root)
	outputDir := filepath.Join(root, "output")
	if err := os.Mkdir(outputDir, 0o700); err != nil {
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
	_, err := FetchAndWrite(context.Background(), FetchOptions{
		Endpoint: server.URL + "/internal/agents/bootstrap", AgentID: testAgentID,
		TokenFile: tokenFile, HTTPClient: server.Client(), OutputDir: outputDir,
		IPQVersion: IPQualityVersion, IPQSHA256: IPQualitySHA256,
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("redirect error = %v, want HTTP 307 rejection", err)
	}
	if redirectedRequests.Load() != 0 {
		t.Fatal("bootstrap client followed a redirect")
	}
}

func encryptedEnvelope(t *testing.T, token []byte, payload Payload) []byte {
	t.Helper()
	plaintext, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(deriveKey(token, payload.AgentID))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := []byte("0123456789ab")
	ciphertext := aead.Seal(nil, nonce, plaintext, []byte(payload.AgentID))
	encoded, err := json.Marshal(fetchResponse{
		Schema:     "akastr-agent-bootstrap.v3",
		Nonce:      base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawURLEncoding.EncodeToString(ciphertext),
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func fetchFixture(t *testing.T, payload Payload, mutate func([]byte) []byte) (Payload, string) {
	t.Helper()
	root := t.TempDir()
	token, raw, tokenFile := testToken(t, root)
	outputDir := filepath.Join(root, "output")
	if err := os.Mkdir(outputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	envelope := encryptedEnvelope(t, raw, payload)
	if mutate != nil {
		envelope = mutate(envelope)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/internal/agents/bootstrap" {
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		var body fetchRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.AgentID != testAgentID || body.MachineToken != token {
			t.Errorf("unexpected request body %#v", body)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(envelope)
	}))
	defer server.Close()
	actual, err := FetchAndWrite(context.Background(), FetchOptions{
		Endpoint: server.URL + "/internal/agents/bootstrap", AgentID: testAgentID,
		TokenFile: tokenFile, HTTPClient: server.Client(), OutputDir: outputDir,
		ConfigurationRoot: "/var/lib/akastr-agent/configurations",
		IPQVersion:        IPQualityVersion, IPQSHA256: IPQualitySHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	return actual, outputDir
}

func TestFetchAndWriteTargetBootstrap(t *testing.T) {
	payload := Payload{
		SchemaVersion: 3, ConfigurationRevision: 1, Mode: "target", AgentID: testAgentID,
		Name: "HKT", ControlEndpoint: "wss://origin.akastrmix.com/internal/agents/ws",
		Target: &Target{
			IPWatchIntervalSeconds: 60,
			ChangeIP:               ChangeIP{Provider: "http_bearer", URL: "https://provider.test/change", BearerToken: "secret-token"},
			SOCKS5:                 SOCKS5{Enabled: true, Port: 38138},
		},
	}
	actual, output := fetchFixture(t, payload, nil)
	if actual.Mode != "target" {
		t.Fatalf("unexpected mode %q", actual.Mode)
	}
	configBytes, err := os.ReadFile(filepath.Join(output, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(configBytes), "secret-token") {
		t.Fatal("provider secret leaked into Agent config")
	}
	if !strings.Contains(string(configBytes), "/var/lib/akastr-agent/configurations/1/changeip-curl.conf") {
		t.Fatal("generated Agent config does not reference its immutable revision directory")
	}
	curlBytes, err := os.ReadFile(filepath.Join(output, "changeip-curl.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(curlBytes), "Authorization: Bearer secret-token") {
		t.Fatal("provider secret was not written to its root-only file")
	}
	if digest, err := os.ReadFile(filepath.Join(output, ConfigurationBootstrapDigestFile)); err != nil || len(strings.TrimSpace(string(digest))) != 64 {
		t.Fatalf("bootstrap digest is missing or invalid: %q err=%v", digest, err)
	}
}

func TestFetchAndWriteRunnerBootstrap(t *testing.T) {
	payload := Payload{
		SchemaVersion: 3, ConfigurationRevision: 1, Mode: "runner", AgentID: testAgentID,
		Name: "Runner", ControlEndpoint: "wss://origin.akastrmix.com/internal/agents/ws",
		Runner: &Runner{Profiles: []ProxyProfile{{ID: "hkt", Username: "proxy-user", Password: "proxy-pass"}}},
	}
	_, output := fetchFixture(t, payload, nil)
	profiles, err := os.ReadFile(filepath.Join(output, "proxy-profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(profiles), `"hkt"`) || !strings.Contains(string(profiles), "proxy-pass") {
		t.Fatal("runner profile file is incomplete")
	}
}

func TestFetchRejectsTamperedCiphertext(t *testing.T) {
	payload := Payload{
		SchemaVersion: 3, ConfigurationRevision: 1, Mode: "runner", AgentID: testAgentID,
		Name: "Runner", ControlEndpoint: "wss://origin.akastrmix.com/internal/agents/ws",
		Runner: &Runner{Profiles: []ProxyProfile{{ID: "hkt", Username: "u", Password: "p"}}},
	}
	root := t.TempDir()
	_, raw, tokenFile := testToken(t, root)
	outputDir := filepath.Join(root, "output")
	if err := os.Mkdir(outputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var envelope fetchResponse
	if err := json.Unmarshal(encryptedEnvelope(t, raw, payload), &envelope); err != nil {
		t.Fatal(err)
	}
	ciphertext, _ := base64.RawURLEncoding.DecodeString(envelope.Ciphertext)
	ciphertext[0] ^= 0xff
	envelope.Ciphertext = base64.RawURLEncoding.EncodeToString(ciphertext)
	responseBody, _ := json.Marshal(envelope)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(responseBody)
	}))
	defer server.Close()
	_, err := FetchAndWrite(context.Background(), FetchOptions{
		Endpoint: server.URL + "/internal/agents/bootstrap", AgentID: testAgentID,
		TokenFile: tokenFile, HTTPClient: server.Client(), OutputDir: outputDir,
		IPQVersion: IPQualityVersion, IPQSHA256: IPQualitySHA256,
	})
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("expected authentication failure, got %v", err)
	}
}

func TestPayloadValidationRejectsGenericShellAndDuplicateProfiles(t *testing.T) {
	target := Payload{
		SchemaVersion: 3, ConfigurationRevision: 1, Mode: "target", AgentID: testAgentID, Name: "Target",
		ControlEndpoint: "wss://origin.akastrmix.com/internal/agents/ws",
		Target:          &Target{IPWatchIntervalSeconds: 60, ChangeIP: ChangeIP{Provider: "command", Program: "/bin/sh"}, SOCKS5: SOCKS5{}},
	}
	if err := target.Validate(testAgentID); err == nil {
		t.Fatal("generic shell command must be rejected")
	}
	target.Target.ChangeIP.Program = "/usr/local/bin/changeip"
	if err := target.Validate(testAgentID); err != nil {
		t.Fatalf("clean absolute command path must be accepted: %v", err)
	}
	target.Target = &Target{
		IPWatchIntervalSeconds: 301,
		ChangeIP:               ChangeIP{Provider: "disabled"},
		SOCKS5:                 SOCKS5{},
	}
	if err := target.Validate(testAgentID); err == nil {
		t.Fatal("IP watch interval above five minutes must be rejected")
	}
	runner := Payload{
		SchemaVersion: 3, ConfigurationRevision: 1, Mode: "runner", AgentID: testAgentID, Name: "Runner",
		ControlEndpoint: "wss://origin.akastrmix.com/internal/agents/ws",
		Runner:          &Runner{Profiles: []ProxyProfile{{ID: "hkt", Username: "u", Password: "p"}, {ID: "hkt", Username: "u2", Password: "p2"}}},
	}
	if err := runner.Validate(testAgentID); err == nil {
		t.Fatal("duplicate profiles must be rejected")
	}
}
