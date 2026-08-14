package release

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	if !strings.Contains(build, "GOARCH=amd64") {
		t.Fatal("release builder must target amd64")
	}
	for _, forbidden := range []string{"arm64", ".sha256", "akastr-agent-linux-$architecture"} {
		if strings.Contains(build, forbidden) {
			t.Fatalf("release builder contains obsolete asset contract %q", forbidden)
		}
	}
	if !strings.Contains(build, "scripts/install.sh > \"$output/install.sh\"") {
		t.Fatal("release builder must render the version-bound interactive installer")
	}
	if !strings.Contains(build, "[ -s \"$output/$asset\" ]") {
		t.Fatal("release builder must fail when the cross-compiler does not create the asset")
	}
}

func TestInstallerKeepsSecretsOutOfTheGeneratedCommandContract(t *testing.T) {
	installer := repositoryFile(t, "scripts", "install.sh")
	for _, required := range []string{
		"@AKASTR_AGENT_VERSION@",
		"@AKASTR_AGENT_BINARY_SHA256@",
		"prompt_secret '一次性 enrollment token",
		"AKASTR_AGENT_MODE",
		"IPQUALITY_COMMIT='0ee5f192fed70c04615852efba0e4b8bd43546c7'",
		"max_concurrency:1",
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("installer missing contract %q", required)
		}
	}
	for _, forbidden := range []string{
		"$asset.sha256",
		"arm64)",
		"eval ",
		"printf 'fail\\nsilent\\nshow-error\\nlocation\\n'",
	} {
		if strings.Contains(installer, forbidden) {
			t.Fatalf("installer contains forbidden contract %q", forbidden)
		}
	}
}
