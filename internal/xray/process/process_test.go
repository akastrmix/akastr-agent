package process

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeXray is a proc directory holding one 3x-ui Xray started with environ.
func fakeXray(t *testing.T, environ string) string {
	t.Helper()
	proc := filepath.Join(t.TempDir(), "proc")
	pid := filepath.Join(proc, "4242")
	if err := os.MkdirAll(pid, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"exe": "/usr/local/x-ui/bin/xray-linux-amd64", "cwd": "/usr/local/x-ui"} {
		if err := os.Symlink(target, filepath.Join(pid, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pid, "environ"), []byte(environ), 0o600); err != nil {
		t.Fatal(err)
	}
	return proc
}

// A node is told it counts Vision downloads as they flow only when Xray would
// read the flag as off: a wrong answer turns fair use judging on for counts
// that still arrive in lumps.
func TestSplicedReadsTheFlagAsXrayDoes(t *testing.T) {
	for environ, want := range map[string]bool{
		"PATH=/usr/bin\x00XRAY_VMESS_AEAD_FORCED=false\x00":            true,
		"XRAY_BUF_SPLICE=disable\x00":                                  false,
		"XRAY_BUF_SPLICE=\x00":                                         false,
		"XRAY_BUF_SPLICE=enable\x00":                                   true,
		"XRAY_BUF_SPLICE=auto":                                         true,
		"XRAY_BUF_SPLICE=disable\x00xray.buf.splice=enable\x00":        true,
		"NOT_XRAY_BUF_SPLICE=disable\x00XRAY_BUF_SPLICE_X=disable\x00": true,
	} {
		x, err := Find(fakeXray(t, environ))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := x.Spliced(); err != nil || got != want {
			t.Fatalf("%q: spliced %v, %v; want %v", environ, got, err, want)
		}
	}
}
