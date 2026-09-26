package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/state"
)

func repositoryFile(t *testing.T, parts ...string) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve release test path")
	}
	path := filepath.Join(append([]string{filepath.Dir(current), "..", ".."}, parts...)...)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(contents)
}

func TestInstallerReadsPersistedIdentityAndBootstrapConfiguration(t *testing.T) {
	bash := installerTestShell(t)
	installer := repositoryFile(t, "scripts", "install.sh")
	start := strings.Index(installer, "read_identity_agent_id() {")
	end := strings.Index(installer, "\nversion_is_newer() {")
	if start < 0 || end <= start {
		t.Fatal("cannot isolate installer identity readers")
	}
	const agentID = "123e4567-e89b-42d3-a456-426614174000"
	root := t.TempDir()
	identityPath := filepath.Join(root, "identity.json")
	configPath := filepath.Join(root, "config.json")
	if err := state.NewJSONFile(identityPath).Save(identity.Identity{
		SchemaVersion:   identity.SchemaVersion,
		EnrollmentState: identity.EnrollmentConfirmed,
		AgentID:         agentID,
		PublicKey:       "a",
		PrivateKey:      "b",
	}); err != nil {
		t.Fatalf("persist identity: %v", err)
	}
	identityContents, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(identityContents), "\n  \"agent_id\":") {
		t.Fatalf("identity fixture does not use the production indented format: %q", identityContents)
	}
	if err := os.WriteFile(configPath, []byte(`{"schema_version":3,"configuration_revision":2,"node":{"id":"`+agentID+`","name":"target"},"control":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	helperPath := filepath.Join(root, "readers.sh")
	helper := installer[start:end] + `
identity_id=$(read_identity_agent_id "$1")
config_id=$(read_config_agent_id "$2")
printf '%s|%s\n' "$identity_id" "$config_id"
`
	if err := os.WriteFile(helperPath, []byte(helper), 0o700); err != nil {
		t.Fatal(err)
	}
	arguments := []string{helperPath, identityPath, configPath}
	output, err := exec.Command(bash, arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("execute installer readers: %v: %s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != agentID+"|"+agentID {
		t.Fatalf("installer readers = %q", got)
	}
}

func installerTestShell(t *testing.T) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal("bash is required for installer tests")
	}
	return bash
}

func TestInstallerStopsUnloadedAndFailedServiceIdempotently(t *testing.T) {
	bash := installerTestShell(t)
	installer := repositoryFile(t, "scripts", "install.sh")
	start := strings.Index(installer, "stop_agent_service() {")
	end := strings.Index(installer, "\nrequire_uuid() {")
	if start < 0 || end <= start {
		t.Fatal("cannot isolate installer service stop function")
	}
	root := t.TempDir()
	helperPath := filepath.Join(root, "stop-service.sh")
	helper := `set -eu
fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }
` + installer[start:end] + `
mode=$1
log=$2
marker=$3
SERVICE_FILE=$4
service_stopped=false
systemctl() {
  printf '%s\n' "$*" >> "$log"
  case "$1" in
    is-enabled) return 1 ;;
    is-active)
      if [ "$mode" = failed ] && [ ! -e "$marker" ]; then printf 'failed\n'; else printf 'unknown\n'; fi
      return 3 ;;
    is-failed) [ "$mode" = failed ] && [ ! -e "$marker" ] ;;
    reset-failed) : > "$marker" ;;
    *) return 0 ;;
  esac
}
stop_agent_service
[ "$service_stopped" = true ]
`
	if err := os.WriteFile(helperPath, []byte(helper), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"unloaded", "failed"} {
		logPath := filepath.Join(root, mode+".log")
		markerPath := filepath.Join(root, mode+".marker")
		servicePath := filepath.Join(root, mode+".service")
		arguments := []string{helperPath, mode, logPath, markerPath, servicePath}
		output, err := exec.Command(bash, arguments...).CombinedOutput()
		if err != nil {
			t.Fatalf("stop %s service: %v: %s", mode, err, output)
		}
		logContents, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		resetCalled := strings.Contains(string(logContents), "reset-failed")
		if resetCalled != (mode == "failed") {
			t.Fatalf("%s service reset-failed called = %v, log = %s", mode, resetCalled, logContents)
		}
	}
}

// The installer and the runtime each carry the pinned IPQuality checksum and
// the Runner command list; a mismatch would install a script or dependency set
// that the runtime then rejects. Installer behaviour itself is covered by the
// Debian container regression in CI.
func TestInstallerAndRuntimeShareRunnerContract(t *testing.T) {
	installer := repositoryFile(t, "scripts", "install.sh")
	model := repositoryFile(t, "internal", "bootstrap", "model.go")
	provider := repositoryFile(t, "internal", "providers", "ipquality", "script", "provider.go")
	extract := func(label, source, pattern string) string {
		t.Helper()
		match := regexp.MustCompile(pattern).FindStringSubmatch(source)
		if match == nil {
			t.Fatalf("cannot find %s", label)
		}
		return match[1]
	}
	installerSHA := extract("installer IPQuality checksum", installer, `(?m)^IPQUALITY_SHA256='([a-f0-9]{64})'$`)
	runtimeSHA := extract("runtime IPQuality checksum", model, `IPQualitySHA256\s*=\s*"([a-f0-9]{64})"`)
	if installerSHA != runtimeSHA {
		t.Fatalf("installer IPQuality checksum %s differs from runtime %s", installerSHA, runtimeSHA)
	}
	installerCommands := strings.Fields(extract("installer Runner commands", installer, `(?m)^RUNNER_COMMANDS='([^']+)'$`))
	var runtimeCommands []string
	for _, quoted := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(
		extract("runtime Runner commands", provider, `var requiredCommands = \[\]string\{([^}]*)\}`), -1) {
		runtimeCommands = append(runtimeCommands, quoted[1])
	}
	if strings.Join(installerCommands, " ") != strings.Join(runtimeCommands, " ") {
		t.Fatalf("installer Runner commands %v differ from runtime %v", installerCommands, runtimeCommands)
	}
}
