package release

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode"
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

func TestReleaseContractIsAmd64OnlyWithoutManualChecksumAssets(t *testing.T) {
	build := repositoryFile(t, "scripts", "build-release.sh")
	powerShellBuild := repositoryFile(t, "scripts", "build-release.ps1")
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
	for _, forbidden := range []string{"arm64", ".sha256"} {
		if strings.Contains(powerShellBuild, forbidden) {
			t.Fatalf("PowerShell release builder contains obsolete contract %q", forbidden)
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
		"wget --no-hsts --https-only --tries=3 --timeout=30 -qO",
		"os_identity=$(",
		"debian:12|debian:13)",
		"wget exit code $wget_code",
		"packages='ca-certificates curl wget'",
		"backup_existing \"$CONFIG_DIR\" \"$CONFIG_BACKUP\"",
		"check-idle --config \"$bootstrap_dir/config.json\"",
		"capture_agent_units",
		"$SYSTEMD_ROOT\"/akastr-agent*.service",
		"$SYSTEMD_ROOT\"/akastr-agent*.timer",
		"ReadWritePaths=/var/lib/akastr-agent /usr/local/lib/akastr-agent",
		"Type=notify",
		"NotifyAccess=main",
		"TimeoutStartSec=45s",
		"Akastr Agent $AGENT_RELEASE_VERSION installed successfully.",
		"rollback_directory \"$CONFIG_BACKUP\" \"$CONFIG_DIR\"",
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
	} {
		if strings.Contains(installer, forbidden) {
			t.Fatalf("installer contains forbidden contract %q", forbidden)
		}
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
