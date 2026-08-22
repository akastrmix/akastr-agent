package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode"

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
	bash, commandPrefix, shellPath := installerTestShell(t)
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
	arguments := append(commandPrefix, shellPath(helperPath), shellPath(identityPath), shellPath(configPath))
	output, err := exec.Command(bash, arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("execute installer readers: %v: %s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != agentID+"|"+agentID {
		t.Fatalf("installer readers = %q", got)
	}
}

func installerTestShell(t *testing.T) (string, []string, func(string) string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		bash, err := exec.LookPath("bash")
		if err != nil {
			t.Skip("bash is unavailable")
		}
		return bash, nil, func(path string) string { return path }
	}

	wsl, err := exec.LookPath("wsl.exe")
	if err != nil {
		t.Skip("WSL is unavailable")
	}
	translate := func(path string) string {
		t.Helper()
		volume := filepath.VolumeName(path)
		if len(volume) != 2 || volume[1] != ':' {
			t.Fatalf("test path is not on a Windows drive: %q", path)
		}
		remainder := strings.TrimPrefix(path, volume)
		return "/mnt/" + strings.ToLower(volume[:1]) + filepath.ToSlash(remainder)
	}
	return wsl, []string{"bash"}, translate
}

func TestInstallerStopsUnloadedAndFailedServiceIdempotently(t *testing.T) {
	bash, commandPrefix, shellPath := installerTestShell(t)
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
		arguments := append(commandPrefix, shellPath(helperPath), mode, shellPath(logPath), shellPath(markerPath), shellPath(servicePath))
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

func TestReleaseContractIsAmd64OnlyWithoutManualChecksumAssets(t *testing.T) {
	build := repositoryFile(t, "scripts", "build-release.sh")
	powerShellBuild := repositoryFile(t, "scripts", "build-release.ps1")
	ci := repositoryFile(t, ".github", "workflows", "ci.yml")
	releaseWorkflow := repositoryFile(t, ".github", "workflows", "release.yml")
	containerIntegration := repositoryFile(t, "scripts", "test-installer-container.sh")
	if !strings.Contains(build, "GOARCH=amd64") {
		t.Fatal("release builder must target amd64")
	}
	for _, forbidden := range []string{"arm64", ".sha256", "akastr-agent-linux-$architecture"} {
		if strings.Contains(build, forbidden) {
			t.Fatalf("release builder contains obsolete asset contract %q", forbidden)
		}
	}
	if !strings.Contains(build, "scripts/install.sh > \"$output/install.sh\"") {
		t.Fatal("release builder must render the version-bound noninteractive installer")
	}
	if !strings.Contains(build, "[ -s \"$output/$asset\" ]") {
		t.Fatal("release builder must fail when the cross-compiler does not create the asset")
	}
	for _, required := range []string{"GOOS = 'linux'", "GOARCH = 'amd64'", "akastr-agent-linux-amd64"} {
		if !strings.Contains(powerShellBuild, required) {
			t.Fatalf("PowerShell release builder missing contract %q", required)
		}
	}
	for _, required := range []string{"go -C $repository build", "Join-Path $repository 'scripts\\install.sh'"} {
		if !strings.Contains(powerShellBuild, required) {
			t.Fatalf("PowerShell release builder must be independent of the caller directory: %q", required)
		}
	}
	for _, forbidden := range []string{"arm64", ".sha256"} {
		if strings.Contains(powerShellBuild, forbidden) {
			t.Fatalf("PowerShell release builder contains obsolete contract %q", forbidden)
		}
	}
	if strings.Contains(powerShellBuild, "Get-FileHash") {
		t.Fatal("PowerShell release builder must not depend on optional hashing cmdlets")
	}
	for _, required := range []string{
		"scripts/build-release.sh v0.0.0",
		`akastr-agent-linux-amd64" version`,
		"bash -n \"$release_root/release/install.sh\"",
		"BINARY_SHA256='$binary_sha'",
		"@AKASTR_AGENT_(VERSION|BINARY_SHA256)@",
	} {
		if !strings.Contains(ci, required) {
			t.Fatalf("CI must execute and verify generated release assets: %q", required)
		}
	}
	for _, required := range []string{
		"go test ./...",
		"go vet ./...",
		"scripts/build-release.sh \"$RELEASE_TAG\" dist",
		`grep -Fq "curl -fsSL --output" dist/install.sh`,
		`! grep -Fq "wget " dist/install.sh`,
	} {
		if !strings.Contains(releaseWorkflow, required) {
			t.Fatalf("release workflow missing current installer contract %q", required)
		}
	}
	if strings.Contains(releaseWorkflow, "go build ./cmd/akastr-agent") {
		t.Fatal("release workflow must not compile a throwaway binary before building exact assets")
	}
	for name, workflow := range map[string]string{"CI": ci, "release": releaseWorkflow} {
		for _, required := range []string{
			"bash -n scripts/test-installer-container.sh",
			"--env AKASTR_INSTALLER_CONTAINER_TEST=1",
			"sh scripts/test-installer-container.sh",
			"for version in 12 13",
		} {
			if !strings.Contains(workflow, required) {
				t.Fatalf("%s workflow missing installer container integration %q", name, required)
			}
		}
	}
	for _, required := range []string{
		`[ "${AKASTR_INSTALLER_CONTAINER_TEST:-}" = '1' ] && [ -e /.dockerenv ]`,
		"run_install target",
		"run_install runner",
		"echo failed > \"$test_root/systemd-state\"",
		"echo unknown > \"$test_root/systemd-state\"",
		"enroll-fail-once",
		"--uninstall --confirm-destroy-local-agent",
		"installer_container_integration_ok",
	} {
		if !strings.Contains(containerIntegration, required) {
			t.Fatalf("installer container integration missing contract %q", required)
		}
	}
}

func TestInstallerUsesOnlySealedNoninteractiveBootstrap(t *testing.T) {
	installer := repositoryFile(t, "scripts", "install.sh")
	for _, character := range installer {
		if unicode.Is(unicode.Han, character) {
			t.Fatal("installer contains non-English user-facing text")
		}
	}
	for _, required := range []string{
		"AGENT_RELEASE_VERSION='@AKASTR_AGENT_VERSION@'",
		"@AKASTR_AGENT_BINARY_SHA256@",
		"AKASTR_AGENT_MACHINE_TOKEN",
		"AKASTR_AGENT_BOOTSTRAP_ENDPOINT",
		" bootstrap \\",
		"--install)",
		"--status)",
		"--uninstall)",
		"IPQUALITY_COMMIT='0ee5f192fed70c04615852efba0e4b8bd43546c7'",
		"IPQUALITY_SHA256='9823c560e0d19769eb627329a31cb47da655d087166d86e40d9b6c77bc7f32fb'",
		"download_https()",
		"curl -fsSL --output",
		"os_identity=$(",
		"debian:12|debian:13)",
		"curl exit code $download_code",
		"RUNNER_PACKAGES='bash bc dnsutils iproute2 jq netcat-openbsd'",
		"RUNNER_COMMANDS='/bin/bash bc curl dig ip jq nc'",
		"command -v \"$command\"",
		"inspect_existing_install \"$agent_id\"",
		"refusing to downgrade Agent",
		"the install command belongs to a different Agent node",
		`"(pending|confirmed)"`,
		"existing identity schema or enrollment state is invalid",
		"install -m 0600 \"$preserved_identity\" \"$CONFIG_DIR/identity.json\"",
		"maintenance_safe_check \"$binary_path\" \"$configuration_dir/config.json\"",
		"CONFIGURATION_ROOT=$STATE_DIR/configurations",
		"ln -s \"$configuration_dir\" \"$deployment_dir/config\"",
		"\"$CONFIG_DIR/changeip-curl.conf\"",
		"\"$CONFIG_DIR/proxy-profiles.json\"",
		"preflight_install",
		"preflight_status",
		"preflight_uninstall",
		"stop_agent_service",
		"systemctl is-failed --quiet akastr-agent.service",
		"fix the reported error and rerun the same command",
		"installed_binary_sha",
		"reuse_ipquality=true",
		"ReadWritePaths=/var/lib/akastr-agent /usr/local/lib/akastr-agent",
		"Type=notify",
		"NotifyAccess=main",
		"TimeoutStartSec=45s",
		"Akastr Agent $AGENT_RELEASE_VERSION installed successfully.",
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("installer missing contract %q", required)
		}
	}
	for _, forbidden := range []string{
		"$asset.sha256",
		"arm64)",
		"AKASTR_AGENT_MODE",
		"/dev/tty",
		"prompt()",
		"prompt_secret",
		"IFS= read",
		"eval ",
		"printf 'fail\\nsilent\\nshow-error\\nlocation\\n'",
		"curl --fail --silent --show-error --location",
		"wget ",
		"\nVERSION='@AKASTR_AGENT_VERSION@'",
		"$RELEASE_BASE_URL/$VERSION/$ASSET",
		"$CONFIG_DIR 已存在；请先核对现有安装",
		"$STATE_DIR 已存在；请先核对现有安装",
		"$RELEASE_ROOT 已存在；请先核对现有安装",
		"akastr-agent-update",
		"--update)",
		"check-identity",
		"previous=$(readlink",
		"self-update",
		"OnCalendar=",
		"RandomizedDelaySec=",
		"已安装",
		"自动更新",
		"systemctl disable --now \"$unit_name\" >/dev/null 2>&1 || true",
		"systemctl daemon-reload >/dev/null 2>&1 || true",
		"systemctl enable \"$unit_name\" >/dev/null 2>&1 || true",
		"systemctl start \"$unit_name\" >/dev/null 2>&1 || true",
		"CONFIG_BACKUP",
		"STATE_BACKUP",
		"RELEASE_BACKUP",
		"UNITS_BACKUP",
		"capture_agent_units",
		"restore_agent_units",
		"rollback_directory",
		"akastr-agent*.service",
		"akastr-agent*.timer",
		"BASE_PACKAGES",
	} {
		if strings.Contains(installer, forbidden) {
			t.Fatalf("installer contains forbidden contract %q", forbidden)
		}
	}
	freshStart := strings.Index(installer, "fresh_install() {")
	freshEnd := strings.Index(installer, "\nshow_status() {")
	if freshStart < 0 || freshEnd <= freshStart {
		t.Fatal("cannot isolate fresh_install")
	}
	freshInstall := installer[freshStart:freshEnd]
	if strings.Index(freshInstall, "inspect_existing_install \"$agent_id\"") > strings.Index(freshInstall, "download_binary") {
		t.Fatal("fresh install must check the existing runtime before downloads or package changes")
	}
	idleCheck := "maintenance_safe_check \"$binary_path\" \"$configuration_dir/config.json\""
	stop := "stop_agent_service"
	if strings.Count(freshInstall, idleCheck) != 2 ||
		strings.Index(freshInstall, idleCheck) > strings.Index(freshInstall, stop) ||
		strings.LastIndex(freshInstall, idleCheck) < strings.Index(freshInstall, stop) {
		t.Fatal("fresh install must run the same maintenance-safe check before and after stopping units")
	}
	uninstallStart := strings.Index(installer, "uninstall_existing() {")
	uninstallEnd := strings.Index(installer, "\noperation=${1:-}\n")
	if uninstallStart < 0 || uninstallEnd <= uninstallStart {
		t.Fatal("cannot isolate uninstall_existing")
	}
	uninstall := installer[uninstallStart:uninstallEnd]
	uninstallIdleCheck := "maintenance_safe_check \"$maintenance_binary\" \"$maintenance_config\""
	if strings.Count(uninstall, uninstallIdleCheck) != 2 ||
		strings.Index(uninstall, uninstallIdleCheck) > strings.Index(uninstall, stop) ||
		strings.LastIndex(uninstall, uninstallIdleCheck) < strings.Index(uninstall, stop) {
		t.Fatal("uninstall must check maintenance safety before and after strictly stopping units")
	}
	main := installer[uninstallEnd:]
	if strings.Index(main, "operation=${1:-}") > strings.Index(main, "preflight_install") {
		t.Fatal("installer must parse its operation before applying mode-specific preflight")
	}
}

func TestReleaseVerifiesPinnedIPQualityRawBytes(t *testing.T) {
	const expected = "9823c560e0d19769eb627329a31cb47da655d087166d86e40d9b6c77bc7f32fb"
	model := repositoryFile(t, "internal", "bootstrap", "model.go")
	verifier := repositoryFile(t, "scripts", "verify-ipquality-source.sh")
	workflow := repositoryFile(t, ".github", "workflows", "release.yml")
	if !strings.Contains(model, `IPQualitySHA256  = "`+expected+`"`) {
		t.Fatal("bootstrap runtime checksum does not match the pinned raw IPQuality source")
	}
	for _, required := range []string{
		"raw.githubusercontent.com/xykt/IPQuality/$commit/ip.sh",
		"sha256sum \"$temporary\"",
		"[ \"$actual\" = \"$expected\" ]",
	} {
		if !strings.Contains(verifier, required) {
			t.Fatalf("IPQuality source verifier missing contract %q", required)
		}
	}
	if !strings.Contains(workflow, "bash scripts/verify-ipquality-source.sh") {
		t.Fatal("release workflow must verify the pinned IPQuality raw bytes before publishing")
	}
}

func TestRunnerDependencyContractIsVerifiedOnEverySupportedDebian(t *testing.T) {
	provider := repositoryFile(t, "internal", "providers", "ipquality", "script", "provider.go")
	verifier := repositoryFile(t, "scripts", "verify-debian-runtime-dependencies.sh")
	ci := repositoryFile(t, ".github", "workflows", "ci.yml")
	release := repositoryFile(t, ".github", "workflows", "release.yml")
	if !strings.Contains(provider, `var requiredCommands = []string{"/bin/bash", "bc", "curl", "dig", "ip", "jq", "nc"}`) {
		t.Fatal("IPQuality runtime command contract changed without updating the installer dependency gate")
	}
	for _, required := range []string{
		"apt-get install -y --no-install-recommends ca-certificates curl $runner_packages",
		"command -v \"$command\"",
		"debian:12|debian:13)",
	} {
		if !strings.Contains(verifier, required) {
			t.Fatalf("Debian dependency verifier missing contract %q", required)
		}
	}
	for name, workflow := range map[string]string{"CI": ci, "release": release} {
		for _, required := range []string{
			"for version in 12 13",
			`"debian:${version}-slim"`,
			"sh scripts/verify-debian-runtime-dependencies.sh",
		} {
			if !strings.Contains(workflow, required) {
				t.Fatalf("%s workflow missing runtime dependency Gate %q", name, required)
			}
		}
	}
}
