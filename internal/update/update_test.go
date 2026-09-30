package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/layout"
	"github.com/akastrmix/akastr-agent/internal/lifecycle"
)

type maintenanceFixture struct {
	PublicKey   string       `json:"public_key"`
	Request     CheckRequest `json:"request"`
	SigningText string       `json:"signing_text"`
	Responses   struct {
		Golden    []namedResponse `json:"golden"`
		Malformed []namedResponse `json:"malformed"`
	} `json:"responses"`
}

type namedResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

// Cloud tests read the same file; a mismatch in either side would stop every
// node from updating.
func TestMaintenanceContractMatchesSharedFixture(t *testing.T) {
	raw, err := os.ReadFile("../protocol/testdata/agent-protocol-v8.json")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Maintenance maintenanceFixture `json:"maintenance"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	fixture := document.Maintenance
	unsigned := fixture.Request
	unsigned.Signature = ""
	if got := string(SigningText(unsigned)); got != fixture.SigningText {
		t.Fatalf("signing text = %q, want %q", got, fixture.SigningText)
	}
	publicKey, _ := base64.RawURLEncoding.DecodeString(fixture.PublicKey)
	signature, _ := base64.RawURLEncoding.DecodeString(fixture.Request.Signature)
	if !ed25519.Verify(publicKey, []byte(fixture.SigningText), signature) {
		t.Fatal("fixture signature does not verify")
	}
	version, revision := fixture.Request.AgentVersion, fixture.Request.ConfigurationRevision
	for _, response := range fixture.Responses.Golden {
		if _, err := decodeTarget(bytes.NewReader(response.Response), version, revision); err != nil {
			t.Errorf("golden response %q rejected: %v", response.Name, err)
		}
	}
	for _, response := range fixture.Responses.Malformed {
		if _, err := decodeTarget(bytes.NewReader(response.Response), version, revision); err == nil {
			t.Errorf("malformed response %q accepted", response.Name)
		}
	}
	if len(fixture.Responses.Golden) == 0 || len(fixture.Responses.Malformed) == 0 {
		t.Fatal("fixture has no responses")
	}
}

type fakeCloud struct {
	mu         sync.Mutex
	target     map[string]any
	errorCodes []string
	binary     []byte
}

func (c *fakeCloud) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if request.URL.Path == "/internal/agents/maintenance" {
		var check CheckRequest
		_ = json.NewDecoder(request.Body).Decode(&check)
		c.errorCodes = append(c.errorCodes, check.ErrorCode)
		_ = json.NewEncoder(response).Encode(c.target)
		return
	}
	_, _ = response.Write(c.binary)
}

// Every request, including the GitHub release download, reaches the fake Cloud.
type redirectTransport struct{ server *httptest.Server }

func (r redirectTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.URL.Host = strings.TrimPrefix(r.server.URL, "https://")
	return r.server.Client().Transport.RoundTrip(request)
}

func TestUpdaterStartsCandidateAndBoundsFailedAttempts(t *testing.T) {
	root := t.TempDir()
	paths := layout.Layout{Root: root, StateDir: filepath.Join(root, "state"), IdentityFile: filepath.Join(root, "identity.json")}
	active := paths.Slot("a")
	if err := os.MkdirAll(active, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.SlotConfig(active), []byte(`{"current":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := paths.Activate(active); err != nil {
		t.Fatal(err)
	}
	newBinary := []byte("candidate binary")
	configuration := `{"schema_version":4,"configuration_revision":4}`
	cloud := &fakeCloud{binary: newBinary, target: map[string]any{
		"schema": CheckSchema, "status": "update_available", "version": "v1.8.0",
		"binary_url":             "https://github.com/akastrmix/akastr-agent/releases/download/v1.8.0/akastr-agent-linux-amd64",
		"binary_sha256":          digest(newBinary),
		"configuration_revision": 4, "configuration": json.RawMessage(configuration),
	}}
	server := httptest.NewTLSServer(cloud)
	defer server.Close()
	client := &http.Client{Transport: redirectTransport{server}}
	credentials := testIdentity(t)
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	var executed []string
	var prepared []string
	updater := &Updater{
		ControlEndpoint: "wss://" + strings.TrimPrefix(server.URL, "https://") + "/internal/agents/ws",
		Identity:        credentials, Version: "v1.7.0", Revision: 3,
		ConfigPath: layout.SlotConfig(active), Layout: paths, Lifecycle: lifecycle.New(),
		Client: Client{HTTPClient: client}, Download: client,
		Run: func(_ context.Context, binary string, args ...string) (string, error) {
			if args[0] == "version" {
				return "v1.8.0\n", nil
			}
			prepared = append(prepared, strings.Join(args, " "))
			return "[]", nil
		},
		Exec: func(binary, configPath string) error {
			executed = append(executed, configPath)
			return nil
		},
		Now: func() time.Time { return now },
	}
	candidate := paths.Slot("b")
	target := TargetName("v1.8.0", 4)

	updater.check(t.Context())
	if len(executed) != 1 || executed[0] != layout.SlotConfig(candidate) {
		t.Fatalf("first attempt executed %v", executed)
	}
	if got, _ := os.ReadFile(layout.SlotBinary(candidate)); !bytes.Equal(got, newBinary) {
		t.Fatal("candidate slot does not hold the downloaded binary")
	}
	if got, _ := os.ReadFile(layout.SlotConfig(candidate)); string(got) != configuration {
		t.Fatalf("candidate configuration = %s", got)
	}
	wantPrepare := "prepare --config " + layout.SlotConfig(candidate) + " --agent-id " + credentials.AgentID + " --revision 4"
	if len(prepared) != 1 || prepared[0] != wantPrepare {
		t.Fatalf("candidate prepare = %v", prepared)
	}
	if active, _ := paths.ActiveSlot(); active != paths.Slot("a") {
		t.Fatal("updater activated the candidate before Cloud accepted it")
	}

	// The candidate failed twice and systemd restarted the active slot each time.
	if err := RecordCandidateFailure(paths, target, "candidate_not_ready"); err != nil {
		t.Fatal(err)
	}
	updater.check(t.Context())
	if err := RecordCandidateFailure(paths, target, "candidate_not_ready"); err != nil {
		t.Fatal(err)
	}
	updater.check(t.Context())
	updater.check(t.Context())
	if len(executed) != 2 {
		t.Fatalf("attempts after two failures = %d, want 2", len(executed))
	}
	if last := cloud.errorCodes[len(cloud.errorCodes)-1]; last != "candidate_not_ready" {
		t.Fatalf("reported error code = %q", last)
	}

	now = now.Add(retryExhaustedAfter)
	updater.check(t.Context())
	if len(executed) != 3 {
		t.Fatal("exhausted target was not retried after the waiting period")
	}
}

func testIdentity(t *testing.T) identity.Identity {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return identity.Identity{
		SchemaVersion: identity.SchemaVersion, AgentID: "123e4567-e89b-42d3-a456-426614174102",
		PublicKey:  base64.RawURLEncoding.EncodeToString(public),
		PrivateKey: base64.RawURLEncoding.EncodeToString(private),
	}
}
