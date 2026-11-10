//go:build linux && amd64

package firewall

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	ipProgram = "/usr/sbin/ip"
	// sysSetns is setns(2) on amd64, the only architecture the Agent ships for.
	sysSetns = 308
)

// nsHost runs the real rules in a network namespace; connection tracking and
// the probe stay out of the host's kernel settings and network.
type nsHost struct{ *system }

func (nsHost) conntrackMax() (int64, bool)                 { return 1 << 40, true }
func (nsHost) raiseConntrack(context.Context, int64) error { return errors.New("not in tests") }
func (nsHost) probe(context.Context) error                 { return nil }

// TestRulesInARealKernel installs rendered rules in a throwaway network
// namespace and checks what they let through. It needs root and nft.
func TestRulesInARealKernel(t *testing.T) {
	if os.Geteuid() != 0 || toolMissing() {
		t.Skip("needs root and nft")
	}
	if _, err := os.Stat(ipProgram); err != nil {
		t.Skip("needs ip")
	}
	node, peer := fmt.Sprintf("akfw-%d-n", os.Getpid()), fmt.Sprintf("akfw-%d-p", os.Getpid())
	t.Cleanup(func() {
		_ = exec.Command(ipProgram, "netns", "del", node).Run()
		_ = exec.Command(ipProgram, "netns", "del", peer).Run()
	})
	for _, args := range [][]string{
		{"netns", "add", node}, {"netns", "add", peer},
		{"netns", "exec", node, ipProgram, "link", "add", "va", "type", "veth", "peer", "name", "vb", "netns", peer},
		{"netns", "exec", node, ipProgram, "addr", "add", "10.201.0.1/24", "dev", "va"},
		{"netns", "exec", peer, ipProgram, "addr", "add", "10.201.0.2/24", "dev", "vb"},
		{"netns", "exec", node, ipProgram, "link", "set", "va", "up"},
		{"netns", "exec", peer, ipProgram, "link", "set", "vb", "up"},
		{"netns", "exec", node, ipProgram, "link", "set", "lo", "up"},
		{"netns", "exec", peer, ipProgram, "link", "set", "lo", "up"},
	} {
		if out, err := exec.Command(ipProgram, args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %v %s", args, err, out)
		}
	}
	system := &system{nft: []string{ipProgram, "netns", "exec", node, NFTProgram}, proc: "/proc"}
	f := newFirewall(nsHost{system}, t.TempDir())
	f.missing = func() bool { return false }
	f.start(context.Background())

	for _, port := range []int{5001, 5002, 5003} {
		echo(t, node, fmt.Sprintf(":%d", port))
	}
	echo(t, peer, ":6001")
	inbound := dial(t, peer, "10.201.0.1:5001")
	outbound := dial(t, node, "10.201.0.2:6001")
	alive := func(name string, c net.Conn, want bool) {
		t.Helper()
		if got := answers(c); got != want {
			t.Fatalf("%s answers = %v, want %v", name, got, want)
		}
	}
	reaches := func(port int) bool {
		var c net.Conn
		var err error
		inNamespace(t, peer, func() { c, err = net.DialTimeout("tcp", fmt.Sprintf("10.201.0.1:%d", port), time.Second) })
		if err == nil {
			c.Close()
		}
		return err == nil
	}
	put := func(mode, sets, blocked, ceilings string) {
		t.Helper()
		raw := `{"mode":"` + mode + `","retry":0,"sets":` + sets + `,"blocked":` + blocked + `,"exempt":null,"ceilings":` + ceilings + `,
			"rules":[{"action":"allow","sources":"any","protocol":"tcp","ports":[[5001,5002]]}]}`
		codes, err := f.Apply(context.Background(), map[string]json.RawMessage{Key: json.RawMessage(raw)})
		if err != nil || codes[Key] != "" {
			t.Fatalf("Apply(%s) = %v, %v", mode, codes, err)
		}
	}
	noSets := `{}`

	// Observe drops nothing that the final rule catches, and records it.
	put("observe", noSets, "null", "[]")
	alive("existing inbound", inbound, true)
	alive("existing outbound", outbound, true)
	if !reaches(5003) {
		t.Fatal("observe dropped a new connection")
	}
	status := &Status{firewall: f, system: system}
	if hits := status.readHits(context.Background()); !strings.Contains(fmt.Sprint(hits.Final), "tcp") {
		t.Fatalf("observe recorded nothing: %+v", hits)
	}

	// Replacing the whole table keeps established connections; enforce drops
	// what no rule allows.
	put("enforce", noSets, "null", "[]")
	alive("existing inbound after replace", inbound, true)
	alive("existing outbound after replace", outbound, true)
	if reaches(5003) || !reaches(5001) {
		t.Fatal("enforce let through what it should not, or blocked an allowed port")
	}

	// A ceiling caps the port even though a manual rule allows it.
	put("enforce", `{"front":{"addresses":["10.201.0.99"],"domains":[]}}`, "null",
		`[{"protocol":"tcp","ports":[[5002,5002]],"sources":["front"]}]`)
	if reaches(5002) || !reaches(5001) {
		t.Fatal("the ceiling did not hold or blocked another port")
	}
	alive("existing inbound under a ceiling", inbound, true)

	// A ban ends the banned source's own connections at once, while this node's
	// connection to it keeps working.
	put("enforce", `{"ban":{"addresses":["10.201.0.2"],"domains":[]}}`, `"ban"`, "[]")
	alive("banned source's connection", inbound, false)
	alive("connection to the banned address", outbound, true)
	alive("new connection to the banned address", dial(t, node, "10.201.0.2:6001"), true)
}

// inNamespace runs fn on a thread inside the named network namespace; sockets
// it creates stay there.
func inNamespace(t *testing.T, name string, fn func()) {
	t.Helper()
	runtime.LockOSThread()
	self, err := os.Open("/proc/thread-self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	defer self.Close()
	target, err := os.Open("/run/netns/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := setns(target.Fd()); err != nil {
		t.Fatal(err)
	}
	fn()
	// A thread that cannot return stays locked and ends with its goroutine.
	if setns(self.Fd()) == nil {
		runtime.UnlockOSThread()
	}
}

func setns(fd uintptr) error {
	if _, _, errno := syscall.RawSyscall(sysSetns, fd, syscall.CLONE_NEWNET, 0); errno != 0 {
		return errno
	}
	return nil
}

// echo answers every line on address inside the namespace.
func echo(t *testing.T, namespace, address string) {
	t.Helper()
	var listener net.Listener
	var err error
	inNamespace(t, namespace, func() { listener, err = net.Listen("tcp4", address) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				reader := bufio.NewReader(c)
				for {
					line, err := reader.ReadString('\n')
					if err != nil || func() error { _, err := c.Write([]byte(line)); return err }() != nil {
						return
					}
				}
			}()
		}
	}()
}

func dial(t *testing.T, namespace, address string) net.Conn {
	t.Helper()
	var c net.Conn
	var err error
	inNamespace(t, namespace, func() { c, err = net.DialTimeout("tcp", address, 2*time.Second) })
	if err != nil {
		t.Fatalf("dial %s from %s: %v", address, namespace, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// answers reports whether an echo comes back within a second.
func answers(c net.Conn) bool {
	_ = c.SetDeadline(time.Now().Add(time.Second))
	if _, err := c.Write([]byte("ping\n")); err != nil {
		return false
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	return err == nil && line == "ping\n"
}
