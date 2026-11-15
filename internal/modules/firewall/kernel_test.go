//go:build linux && amd64

package firewall

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
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

// nsHost runs the real rules in a network namespace; connection tracking,
// forwarding and the probe stay out of the host's kernel settings and network.
type nsHost struct {
	*system
	namespace string
}

func (nsHost) conntrackMax() (int64, bool)                 { return 1 << 40, true }
func (nsHost) raiseConntrack(context.Context, int64) error { return errors.New("not in tests") }
func (nsHost) probe(context.Context) error                 { return nil }
func (nsHost) transit() ([]string, []string)               { return []string{"eth0"}, nil }

func (h nsHost) forwarding() (bool, error) {
	out, err := exec.Command(ipProgram, "netns", "exec", h.namespace, sysctl, "-n", "net.ipv4.ip_forward").Output()
	return strings.TrimSpace(string(out)) == "1", err
}

func (h nsHost) setForwarding(_ context.Context, on bool) error {
	value := "0"
	if on {
		value = "1"
	}
	return exec.Command(ipProgram, "netns", "exec", h.namespace, sysctl, "-qw", "net.ipv4.ip_forward="+value).Run()
}

// kernelReady skips unless everything the rules use is already in the kernel:
// these tests run on shared machines and never load a module. Without nft_fib
// the guard that translates only packets for this node becomes the node's
// address, which is the same thing here.
func kernelReady(t *testing.T, node string) {
	t.Helper()
	if os.Geteuid() != 0 || toolMissing() {
		t.Skip("needs root and nft")
	}
	if _, err := os.Stat(ipProgram); err != nil {
		t.Skip("needs ip")
	}
	for _, module := range []string{"nf_tables", "nf_conntrack", "nf_conntrack_netlink", "nf_nat", "nft_chain_nat",
		"nft_nat", "nft_masq", "nft_ct", "nft_socket", "veth", "bridge"} {
		if _, err := os.Stat("/sys/module/" + module); err != nil {
			t.Skipf("kernel module %s is not loaded, and tests never load modules", module)
		}
	}
	for _, module := range []string{"nft_fib", "nft_fib_inet", "nft_fib_ipv4", "nft_fib_ipv6"} {
		if _, err := os.Stat("/sys/module/" + module); err != nil {
			guard := localGuard
			localGuard = "ip daddr " + node
			t.Cleanup(func() { localGuard = guard })
			t.Logf("%s not loaded: the local-destination guard is ip daddr %s", module, node)
			return
		}
	}
}

func inSpace(t *testing.T, namespace string) func() (int, error) {
	return func() (int, error) {
		var fd int
		var err error
		inNamespace(t, namespace, func() { fd, err = openNetlink() })
		return fd, err
	}
}

// TestRulesInARealKernel installs rendered rules in a throwaway network
// namespace and checks what they let through. It needs root and nft.
func TestRulesInARealKernel(t *testing.T) {
	kernelReady(t, "10.201.0.1")
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
	system := &system{nft: []string{ipProgram, "netns", "exec", node, NFTProgram}, proc: "/proc", sys: "/sys"}
	system.netlink = inSpace(t, node)
	f := newFirewall(nsHost{system, node}, t.TempDir())
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

	// Observe drops nothing that the final rule catches, and records it; a
	// port nothing listens on is a scanner's guess and stays out of the record.
	put("observe", noSets, "null", "[]")
	alive("existing inbound", inbound, true)
	alive("existing outbound", outbound, true)
	if !reaches(5003) {
		t.Fatal("observe dropped a new connection")
	}
	reaches(5009)
	status := &Status{firewall: f, system: system}
	recorded := map[int]bool{}
	for _, hit := range status.readHits(context.Background()).Final {
		if hit.Protocol == "tcp" && hit.Port != nil {
			recorded[*hit.Port] = true
		}
	}
	if !recorded[5003] || recorded[5009] {
		t.Fatalf("observe recorded %v, want the listening 5003 and not the closed 5009", recorded)
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

// TestForwardingInARealKernel puts a client, the node and two landings on one
// link and forwards through the node with the real rules.
func TestForwardingInARealKernel(t *testing.T) {
	kernelReady(t, "10.202.0.1")
	prefix := fmt.Sprintf("akfw-%d-", os.Getpid())
	space := func(name string) string { return prefix + name }
	for _, name := range []string{"sw", "c", "n", "a", "b"} {
		t.Cleanup(func() { _ = exec.Command(ipProgram, "netns", "del", space(name)).Run() })
	}
	ip := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(ipProgram, args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %v %s", args, err, out)
		}
	}
	for _, name := range []string{"sw", "c", "n", "a", "b"} {
		ip("netns", "add", space(name))
		ip("-n", space(name), "link", "set", "lo", "up")
	}
	ip("-n", space("sw"), "link", "add", "br0", "type", "bridge")
	ip("-n", space("sw"), "link", "set", "br0", "up")
	hosts := map[string]string{"c": "10.202.0.10", "n": "10.202.0.1", "a": "10.202.0.20", "b": "10.202.0.30"}
	for name, address := range hosts {
		ip("-n", space("sw"), "link", "add", "p-"+name, "type", "veth", "peer", "name", "eth0", "netns", space(name))
		ip("-n", space("sw"), "link", "set", "p-"+name, "master", "br0", "up")
		ip("-n", space(name), "addr", "add", address+"/24", "dev", "eth0")
		ip("-n", space(name), "link", "set", "eth0", "up")
	}
	ip("-n", space("n"), "route", "add", "default", "via", "10.202.0.254")
	// The client uses the node as its router to landing b: the transit check.
	ip("-n", space("c"), "route", "add", hosts["b"]+"/32", "via", hosts["n"])
	for _, setting := range []string{"net.ipv4.ip_forward=0", "net.ipv4.conf.all.send_redirects=0", "net.ipv4.conf.eth0.send_redirects=0"} {
		ip("netns", "exec", space("n"), sysctl, "-qw", setting)
	}
	for _, setting := range []string{"net.ipv4.conf.all.accept_redirects=0", "net.ipv4.conf.eth0.accept_redirects=0"} {
		ip("netns", "exec", space("c"), sysctl, "-qw", setting)
	}

	for _, landing := range []string{"a", "b"} {
		tag := strings.ToUpper(landing)
		taggedEcho(t, space(landing), "tcp", ":5000", tag)
		taggedEcho(t, space(landing), "udp", ":5000", tag)
	}
	taggedEcho(t, space("b"), "tcp", ":6000", "B")
	taggedEcho(t, space("n"), "tcp", ":7000", "N")

	system := &system{nft: []string{ipProgram, "netns", "exec", space("n"), NFTProgram}, proc: "/proc", sys: "/sys"}
	system.netlink = inSpace(t, space("n"))
	host := nsHost{system, space("n")}
	// Every node arrives here with a table from before forwarding: its marker
	// sits in input. It reads as unmarked and is replaced.
	if err := system.load(context.Background(), "table inet akastr_firewall {\n  chain input {\n"+
		"    type filter hook input priority filter; policy accept;\n"+
		"    iif \"lo\" accept comment \"akastr:enforce:00000000000000000000000000000000\"\n  }\n}\n"); err != nil {
		t.Fatal(err)
	}
	if installed, err := system.installed(context.Background()); err != nil || installed != "unmarked" {
		t.Fatalf("a table from before forwarding reads as %q, %v", installed, err)
	}
	f := newFirewall(host, t.TempDir())
	f.missing = func() bool { return false }
	f.start(context.Background())
	// put applies twice, as the five-minute round does.
	put := func(mode, blocked string, forwards ...string) {
		t.Helper()
		sets := `{}`
		ban := "null"
		if blocked != "" {
			sets, ban = `{"ban":{"addresses":["`+blocked+`"],"domains":[]}}`, `"ban"`
		}
		raw := `{"mode":"` + mode + `","retry":0,"sets":` + sets + `,"blocked":` + ban + `,"exempt":null,"ceilings":[],
			"rules":[{"action":"allow","sources":"any","protocol":"tcp","ports":[[22,22]]}],"forwards":[` + strings.Join(forwards, ",") + `]}`
		for range 2 {
			codes, err := f.Apply(context.Background(), map[string]json.RawMessage{Key: json.RawMessage(raw)})
			if err != nil || codes[Key] != "" {
				t.Fatalf("Apply(%s %v) = %v, %v", mode, forwards, codes, err)
			}
		}
	}
	to := func(landing, protocol string) string {
		return `{"protocol":"` + protocol + `","port":5000,"to":"` + hosts[landing] + `","to_port":5000}`
	}
	both := func(landing string) []string { return []string{to(landing, "tcp"), to(landing, "udp")} }
	node := hosts["n"] + ":5000"
	forwarded := func(t *testing.T) string {
		t.Helper()
		tag, _ := ask(t, space("c"), "tcp", node)
		return tag
	}

	// Through the map, the landing sees the node's address.
	put("enforce", "", both("a")...)
	if installed, err := system.installed(context.Background()); err != nil || !strings.HasPrefix(installed, "akastr:enforce:") {
		t.Fatalf("the replaced table reads as %q, %v", installed, err)
	}
	if on, _ := host.forwarding(); !on {
		t.Fatal("forwarding stayed off")
	}
	tcpFlow, udpFlow := flow(t, space("c"), "tcp", node), flow(t, space("c"), "udp", node)
	for name, c := range map[string]*tagged{"tcp": tcpFlow, "udp": udpFlow} {
		if tag, peer := c.ask(); tag != "A" || peer != hosts["n"] {
			t.Fatalf("%s through the forward: %q from the landing's view of %q", name, tag, peer)
		}
	}
	// Changing members keeps live connections.
	put("enforce", "", append(both("a"), `{"protocol":"tcp","port":5001,"to":"`+hosts["b"]+`","to_port":5001}`)...)
	if tag, _ := tcpFlow.ask(); tag != "A" {
		t.Fatalf("live TCP after a member change: %q", tag)
	}
	if tag, _ := udpFlow.ask(); tag != "A" {
		t.Fatalf("live UDP after a member change: %q", tag)
	}

	// The landing moves: the old UDP flow follows with its next packet, and
	// the node's own connections to the old landing are not forwards.
	ownTCP, ownUDP := flow(t, space("n"), "tcp", hosts["a"]+":5000"), flow(t, space("n"), "udp", hosts["a"]+":5000")
	for _, c := range []*tagged{ownTCP, ownUDP} {
		c.ask()
	}
	put("enforce", "", both("b")...)
	// Deleted, they would come back on the node's next packet, but what the
	// landing sends first meanwhile would be a new connection and dropped.
	if own := trackedFrom(t, system, hosts["a"]+":5000"); own != 2 {
		t.Fatalf("the node's own connections to the old landing left in tracking: %d, want 2", own)
	}
	if tag, _ := udpFlow.ask(); tag != "B" {
		t.Fatalf("old UDP flow after the landing moved: %q", tag)
	}
	if tag, _ := tcpFlow.ask(); tag == "A" {
		t.Fatal("old TCP connection still reaches the old landing")
	}
	if tag := forwarded(t); tag != "B" {
		t.Fatalf("new connection after the landing moved: %q", tag)
	}
	for name, c := range map[string]*tagged{"tcp": ownTCP, "udp": ownUDP} {
		if tag, _ := c.ask(); tag != "A" {
			t.Fatalf("the node's own %s connection to the old landing: %q", name, tag)
		}
	}

	// The forward goes: the old flow stops reaching the landing, and
	// forwarding goes back off.
	put("enforce", "")
	if tag, _ := udpFlow.ask(); tag == "B" {
		t.Fatal("old UDP flow still reaches the landing of a removed forward")
	}
	if on, _ := host.forwarding(); on {
		t.Fatal("forwarding left on")
	}

	// A ban holds before translation.
	put("enforce", "", both("a")...)
	live := flow(t, space("c"), "tcp", node)
	put("enforce", hosts["c"], both("a")...)
	if tag, _ := live.ask(); tag != "" {
		t.Fatal("ban left the banned client's forwarded connection")
	}
	if tag := forwarded(t); tag != "" {
		t.Fatal("banned client used the forward")
	}

	// Someone using the node as a router gets nowhere, on any port, and is
	// not redirected to a landing either.
	put("enforce", "", both("a")...)
	for _, port := range []string{"6000", "5000"} {
		if tag, _ := ask(t, space("c"), "tcp", hosts["b"]+":"+port); tag != "" {
			t.Fatalf("transit to b:%s through the node answered by %q", port, tag)
		}
	}
	if tag := forwarded(t); tag != "A" {
		t.Fatalf("forward beside the transit check: %q", tag)
	}

	// Off still forwards and filters nothing; off without forwards is no table.
	put("off", "", both("a")...)
	if tag := forwarded(t); tag != "A" {
		t.Fatalf("forward in mode off: %q", tag)
	}
	if tag, _ := ask(t, space("c"), "tcp", hosts["n"]+":7000"); tag != "N" {
		t.Fatal("mode off filtered a local service")
	}
	put("off", "")
	if installed, err := system.installed(context.Background()); err != nil || installed != "" {
		t.Fatalf("off without forwards left a table: %q %v", installed, err)
	}
	if on, _ := host.forwarding(); on {
		t.Fatal("forwarding left on after the table went")
	}
}

// taggedEcho answers each TCP line or UDP datagram with "tag peer".
func taggedEcho(t *testing.T, namespace, network, address, tag string) {
	t.Helper()
	if network == "udp" {
		var pc net.PacketConn
		var err error
		inNamespace(t, namespace, func() { pc, err = net.ListenPacket("udp4", address) })
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pc.Close() })
		go func() {
			buf := make([]byte, 512)
			for {
				n, peer, err := pc.ReadFrom(buf)
				if err != nil {
					return
				}
				host, _, _ := net.SplitHostPort(peer.String())
				_, _ = pc.WriteTo([]byte(tag+" "+host+" "+string(buf[:n])), peer)
			}
		}()
		return
	}
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
				host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
				reader := bufio.NewReader(c)
				for {
					line, err := reader.ReadString('\n')
					if err != nil || func() error { _, err := c.Write([]byte(tag + " " + host + " " + line)); return err }() != nil {
						return
					}
				}
			}()
		}
	}()
}

// tagged is one live connection or UDP flow; ask returns who answered it.
type tagged struct {
	conn   net.Conn
	reader *bufio.Reader
	udp    bool
	seq    int
}

func flow(t *testing.T, namespace, network, address string) *tagged {
	t.Helper()
	var c net.Conn
	var err error
	inNamespace(t, namespace, func() { c, err = net.DialTimeout(network+"4", address, time.Second) })
	if err != nil {
		t.Fatalf("dial %s %s from %s: %v", network, address, namespace, err)
	}
	t.Cleanup(func() { c.Close() })
	return &tagged{conn: c, reader: bufio.NewReader(c), udp: network == "udp"}
}

// ask opens a new connection and asks once; "" when nobody answers.
func ask(t *testing.T, namespace, network, address string) (string, string) {
	t.Helper()
	var c net.Conn
	var err error
	inNamespace(t, namespace, func() { c, err = net.DialTimeout(network+"4", address, time.Second) })
	if err != nil {
		return "", ""
	}
	defer c.Close()
	return (&tagged{conn: c, reader: bufio.NewReader(c), udp: network == "udp"}).ask()
}

func (c *tagged) ask() (tag, peer string) {
	c.seq++
	payload := strconv.Itoa(c.seq)
	deadline := time.Now().Add(time.Second)
	_ = c.conn.SetDeadline(deadline)
	if c.udp {
		if _, err := c.conn.Write([]byte(payload)); err != nil {
			return "", ""
		}
		buf := make([]byte, 512)
		for time.Now().Before(deadline) {
			n, err := c.conn.Read(buf)
			if errors.Is(err, syscall.ECONNREFUSED) {
				continue
			}
			if err != nil {
				return "", ""
			}
			if fields := strings.Fields(string(buf[:n])); len(fields) == 3 && fields[2] == payload {
				return fields[0], fields[1]
			}
		}
		return "", ""
	}
	if _, err := c.conn.Write([]byte(payload + "\n")); err != nil {
		return "", ""
	}
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return "", ""
		}
		if fields := strings.Fields(line); len(fields) == 3 && fields[2] == payload {
			return fields[0], fields[1]
		}
	}
}

// trackedFrom counts the untranslated tracked connections whose replies come
// from address.
func trackedFrom(t *testing.T, s *system, address string) int {
	t.Helper()
	fd, err := s.netlink()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	from := netip.MustParseAddrPort(address)
	count := 0
	err = (&ctNetlink{fd: fd}).exchange(ctGet, syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP, nil, func(payload []byte) {
		if e, ok := parseTracked(payload); ok && e.reply == from && e.status&ipsDstNAT == 0 {
			count++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}
