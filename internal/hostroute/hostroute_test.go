package hostroute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultRoutes(t *testing.T) {
	proc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proc, "net"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(proc, "net", "route"), []byte(
		"Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
			"eth0\t00000000\t01DEB52F\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"+
			"eth0\t00DEB52F\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"), 0o600)
	os.WriteFile(filepath.Join(proc, "net", "ipv6_route"), []byte(
		"00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003 ens4\n"+
			"00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200 lo\n"), 0o600)
	v4, v6, err := Defaults(proc)
	if err != nil || strings.Join(v4, ",") != "eth0" || strings.Join(v6, ",") != "ens4" {
		t.Fatalf("Defaults = %v %v %v", v4, v6, err)
	}
}
