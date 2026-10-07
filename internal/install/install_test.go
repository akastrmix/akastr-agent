package install

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/layout"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const testAgentID = "123e4567-e89b-42d3-a456-426614174102"

type fakeControl struct {
	token       []byte
	payload     []byte
	enrolledKey string
}

func (c *fakeControl) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/internal/agents/bootstrap":
		extract := hmac.New(sha256.New, []byte(testAgentID))
		extract.Write(c.token)
		expand := hmac.New(sha256.New, extract.Sum(nil))
		expand.Write([]byte("akastr-agent-bootstrap-v3"))
		expand.Write([]byte{1})
		block, _ := aes.NewCipher(expand.Sum(nil))
		aead, _ := cipher.NewGCM(block)
		nonce := []byte("0123456789ab")
		_ = json.NewEncoder(response).Encode(map[string]string{
			"schema": "akastr-agent-bootstrap.v4", "nonce": base64.RawURLEncoding.EncodeToString(nonce),
			"ciphertext": base64.RawURLEncoding.EncodeToString(aead.Seal(nil, nonce, c.payload, []byte(testAgentID))),
		})
	case "/internal/agents/enroll":
		var body struct {
			PublicKey string `json:"public_key"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		c.enrolledKey = body.PublicKey
		_ = json.NewEncoder(response).Encode(map[string]any{"ok": true, "agent_id": testAgentID, "protocol": protocol.Version})
	default:
		http.NotFound(response, request)
	}
}

func testInstall(t *testing.T, root string) (Options, *fakeControl, *[]string) {
	t.Helper()
	server := httptest.NewTLSServer(nil)
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "https://")
	token := bytes.Repeat([]byte{3}, 32)
	control := &fakeControl{token: token, payload: []byte(fmt.Sprintf(`{"schema_version":5,"configuration_revision":5,
"agent_id":%q,"name":"HKT","control_endpoint":"wss://%s/internal/agents/ws",
"modules":{"ip_watch":{"interval_seconds":60,"ipv6":false}}}`, testAgentID, host))}
	server.Config.Handler = control
	executable := filepath.Join(root, "installer")
	if err := os.WriteFile(executable, []byte("release binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	var commands []string
	return Options{
		AgentID: testAgentID, MachineToken: base64.RawURLEncoding.EncodeToString(token),
		BootstrapEndpoint: server.URL + "/internal/agents/bootstrap", Version: "v1.7.0",
		Executable: executable, UnitFile: filepath.Join(root, "akastr-agent.service"),
		Layout: layout.Layout{
			Root: filepath.Join(root, "lib"), StateDir: filepath.Join(root, "state"),
			IdentityFile: filepath.Join(root, "etc", "identity.json"),
		},
		HTTPClient: server.Client(), Output: &bytes.Buffer{},
		System: func(_ context.Context, name string, args ...string) error {
			commands = append(commands, name+" "+strings.Join(args, " "))
			return nil
		},
	}, control, &commands
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Reinstalling keeps the execution journal and IP facts that prevent repeated
// ChangeIP side effects, and replaces everything else.
func TestReinstallKeepsExecutionState(t *testing.T) {
	root := t.TempDir()
	options, control, commands := testInstall(t, root)
	paths := options.Layout
	journal := `{"schema_version":1,"active":{},"recent":[]}`
	writeFile(t, paths.StateFile(), journal)
	writeFile(t, paths.IdentityFile, `{"schema_version":2,"enrollment_state":"confirmed","agent_id":"`+testAgentID+`"}`)
	writeFile(t, options.UnitFile, "old unit")

	if err := Install(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(paths.StateFile()); string(got) != journal {
		t.Fatal("reinstall changed the operation journal")
	}
	installed, err := identity.Load(paths.IdentityFile)
	if err != nil || installed.PublicKey != control.enrolledKey {
		t.Fatalf("installed identity %v does not match the enrolled key", err)
	}
	if active, _ := paths.ActiveSlot(); active != paths.Slot("a") {
		t.Fatalf("active slot = %q", active)
	}
	if got, _ := os.ReadFile(layout.SlotConfig(paths.Current())); !bytes.Equal(got, control.payload) {
		t.Fatal("installed configuration differs from Cloud's configuration")
	}
	unit, _ := os.ReadFile(options.UnitFile)
	if !strings.Contains(string(unit), "ExecStart="+paths.Current()+"/akastr-agent run --config "+paths.Current()+"/config.json") {
		t.Fatalf("unit does not start the current slot:\n%s", unit)
	}
	want := []string{"systemctl stop akastr-agent.service", "systemctl restart akastr-agent.service"}
	for _, command := range want {
		if !strings.Contains(strings.Join(*commands, "\n"), command) {
			t.Fatalf("commands %v miss %q", *commands, command)
		}
	}
}

func TestInstallRefusesAnotherNodesMachine(t *testing.T) {
	root := t.TempDir()
	options, control, _ := testInstall(t, root)
	writeFile(t, options.Layout.IdentityFile, `{"schema_version":3,"agent_id":"2bfadfbb-7481-4d96-9e0b-40a04aa4aeb4"}`)
	if err := Install(t.Context(), options); err == nil || !strings.Contains(err.Error(), "another Agent node") {
		t.Fatalf("Install() error = %v", err)
	}
	if control.enrolledKey != "" {
		t.Fatal("install enrolled on another node's machine")
	}
}

func TestInstallRefusesStateWithoutIdentity(t *testing.T) {
	root := t.TempDir()
	options, control, _ := testInstall(t, root)
	writeFile(t, filepath.Join(options.Layout.StateDir, "changeip-reconciliation.json"), `{}`)
	if err := Install(t.Context(), options); err == nil || control.enrolledKey != "" {
		t.Fatalf("Install() error = %v enrolled=%q", err, control.enrolledKey)
	}
}
