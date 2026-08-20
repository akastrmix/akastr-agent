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

func TestReleaseContractIsAmd64OnlyWithoutManualChecksumAssets(t *testing.T) {
	build := repositoryFile(t, "scripts", "build-release.sh")
	powerShellBuild := repositoryFile(t, "scripts", "build-release.ps1")
	ci := repositoryFile(t, ".github", "workflows", "ci.yml")
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
		"curl --fail --location --silent --show-error --proto '=https' --tlsv1.2",
		"wget --no-hsts --https-only --tries=3 --timeout=30 -qO",
		"os_identity=$(",
		"debian:12|debian:13)",
		"wget exit code $download_code",
		"BASE_PACKAGES='ca-certificates curl wget'",
		"RUNNER_PACKAGES='bash bc dnsutils iproute2 jq netcat-openbsd'",
		"RUNNER_COMMANDS='/bin/bash bc curl dig ip jq nc'",
		"command -v \"$command\"",
		"backup_existing \"$CONFIG_DIR\" \"$CONFIG_BACKUP\"",
		"inspect_existing_install \"$agent_id\"",
		"refusing to downgrade Agent",
		"the install command belongs to a different Agent node",
		`"(pending|confirmed)"`,
		"existing identity schema or enrollment state is invalid",
		"install -m 0600 \"$preserved_identity\" \"$CONFIG_DIR/identity.json\"",
		"maintenance_safe_check \"$binary_path\" \"$bootstrap_dir/config.json\"",
		"preflight_install",
		"preflight_status",
		"preflight_uninstall",
		"capture_agent_units",
		"$SYSTEMD_ROOT\"/akastr-agent*.service",
		"$SYSTEMD_ROOT\"/akastr-agent*.timer",
		"ReadWritePaths=/var/lib/akastr-agent /usr/local/lib/akastr-agent",
		"Type=notify",
		"NotifyAccess=main",
		"TimeoutStartSec=45s",
		"Akastr Agent $AGENT_RELEASE_VERSION installed successfully.",
		"rollback_directory \"$CONFIG_BACKUP\" \"$CONFIG_DIR\"",
		"installer rollback or cleanup was incomplete",
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
		"wget -qO-",
		"wget --https-only --tries=3 --timeout=30 -qO",
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
	idleCheck := "maintenance_safe_check \"$binary_path\" \"$bootstrap_dir/config.json\""
	stop := "capture_agent_units"
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
		"apt-get install -y --no-install-recommends $base_packages $runner_packages",
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
