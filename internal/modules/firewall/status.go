package firewall

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const (
	// KindStatus reports the node's firewall facts whenever the module is on,
	// including in mode off, so Cloud previews with the real ports.
	KindStatus       = "firewall.status"
	statusInterval   = time.Minute
	maxListening     = 256
	maxSSHPorts      = 16
	maxForeignTables = 32
)

type statusData struct {
	Mode          string         `json:"mode"`
	SSHPorts      []int          `json:"ssh_ports"`
	PanelPort     *int           `json:"panel_port"`
	Listening     []listening    `json:"listening"`
	Conntrack     *conntrackUse  `json:"conntrack"`
	Hits          hits           `json:"hits"`
	Domains       []domainStatus `json:"domains"`
	ForeignTables []string       `json:"foreign_tables"`
}

type listening struct {
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
}

type conntrackUse struct {
	Count int64 `json:"count"`
	Max   int64 `json:"max"`
}

type hits struct {
	Final     []finalHit   `json:"final"`
	Ceiling   []ceilingHit `json:"ceiling"`
	Truncated bool         `json:"truncated"`
}

// finalHit is what the default caught: a port for TCP and UDP, a message type
// for ICMP, the protocol alone otherwise.
type finalHit struct {
	Protocol string `json:"protocol"`
	Port     *int   `json:"port,omitempty"`
	Type     *int   `json:"type,omitempty"`
	Packets  int64  `json:"packets"`
}

type ceilingHit struct {
	Source   string `json:"source"`
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	Packets  int64  `json:"packets"`
}

type domainStatus struct {
	Name       string     `json:"name"`
	Addresses  []string   `json:"addresses"`
	ResolvedAt *time.Time `json:"resolved_at"`
	Failed     bool       `json:"failed"`
}

// Status sends firewall.status each minute. Only the newest matters, so an
// unacknowledged one is replaced, never queued.
type Status struct {
	firewall  *Firewall
	system    *system
	panelPort *int
	wake      chan struct{}

	mu       sync.Mutex
	pending  *protocol.ReportBody
	pendData []byte
}

var _ module.Reporter = (*Status)(nil)

// NewStatus reports for f; panelPort is the 3x-ui panel's port when the node
// runs xui.
func NewStatus(f *Firewall, panelPort *int) *Status {
	return &Status{firewall: f, system: newSystem(""), panelPort: panelPort, wake: make(chan struct{}, 1)}
}

func (s *Status) Run(ctx context.Context, publish module.Publish) error {
	ticker := time.NewTicker(statusInterval)
	defer ticker.Stop()
	for {
		s.tick(ctx, publish)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

func (s *Status) tick(ctx context.Context, publish module.Publish) {
	data, err := json.Marshal(s.read(ctx))
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.pending == nil || !bytes.Equal(s.pendData, data) {
		s.pending = &protocol.ReportBody{ReportID: protocol.NewUUID(), Kind: KindStatus, Data: json.RawMessage(data)}
		s.pendData = data
	}
	report := *s.pending
	s.mu.Unlock()
	_ = publish(report)
}

func (s *Status) ControlReady() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Status) Acknowledge(reportID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil || s.pending.ReportID != reportID {
		return false, nil
	}
	s.pending, s.pendData = nil, nil
	return true, nil
}

func (s *Status) UpdateSafe() error { return nil }

func (s *Status) read(ctx context.Context) statusData {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	data := statusData{
		Mode: modeOff, SSHPorts: []int{}, PanelPort: s.panelPort, Listening: []listening{},
		Hits:    hits{Final: []finalHit{}, Ceiling: []ceilingHit{}},
		Domains: s.firewall.domains(), ForeignTables: []string{},
	}
	sockets, err := readSockets(s.system.proc)
	if err != nil {
		slog.Warn("listening sockets unreadable", "error", err.Error())
	}
	data.Listening, data.SSHPorts = listeningPorts(sockets), sshPorts(s.system.proc, sockets)
	count, countOK := s.system.conntrackCount()
	limit, limitOK := s.system.conntrackMax()
	if countOK && limitOK {
		data.Conntrack = &conntrackUse{Count: count, Max: limit}
	}
	if toolMissing() || !nftablesLoaded() {
		return data
	}
	tables, err := s.system.tables(ctx)
	if err != nil {
		return data
	}
	present := false
	for _, t := range tables {
		if t == [2]string{"inet", Table} {
			present = true
		} else if len(data.ForeignTables) < maxForeignTables {
			data.ForeignTables = append(data.ForeignTables, t[0]+" "+t[1])
		}
	}
	slices.Sort(data.ForeignTables)
	if !present {
		return data
	}
	if installed, err := s.system.installed(ctx); err == nil {
		if mode, _, ok := parseMarker(installed); ok {
			data.Mode = mode
		}
	}
	data.Hits = s.readHits(ctx)
	return data
}

func (s *Status) readHits(ctx context.Context) hits {
	result := hits{Final: []finalHit{}, Ceiling: []ceilingHit{}}
	for _, name := range slices.Sorted(maps.Keys(hitSizes)) {
		out, err := s.system.run(ctx, "", "-j", "-n", "list", "set", "inet", Table, name)
		if err != nil {
			continue
		}
		elements, size := setElements(out)
		result.Truncated = result.Truncated || (size > 0 && len(elements) >= size)
		for _, e := range elements {
			switch name {
			case hitFinal:
				if len(e.values) == 2 {
					port := intOf(e.values[1])
					result.Final = append(result.Final, finalHit{Protocol: protocolName(e.values[0]), Port: &port, Packets: e.packets})
				}
			case hitICMP4, hitICMP6:
				if len(e.values) == 1 {
					kind, protocol := intOf(e.values[0]), "icmp"
					if name == hitICMP6 {
						protocol = "icmpv6"
					}
					result.Final = append(result.Final, finalHit{Protocol: protocol, Type: &kind, Packets: e.packets})
				}
			case hitProtocol:
				if len(e.values) == 1 {
					result.Final = append(result.Final, finalHit{Protocol: protocolName(e.values[0]), Packets: e.packets})
				}
			case hitCeiling4, hitCeiling6:
				var source string
				if len(e.values) == 3 && json.Unmarshal(e.values[0], &source) == nil {
					result.Ceiling = append(result.Ceiling, ceilingHit{
						Source: source, Protocol: protocolName(e.values[1]), Port: intOf(e.values[2]), Packets: e.packets,
					})
				}
			}
		}
	}
	slices.SortFunc(result.Final, func(a, b finalHit) int {
		return cmp.Or(cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(deref(a.Port), deref(b.Port)), cmp.Compare(deref(a.Type), deref(b.Type)))
	})
	slices.SortFunc(result.Ceiling, func(a, b ceilingHit) int {
		return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(a.Port, b.Port))
	})
	return result
}

func deref(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

type element struct {
	values  []json.RawMessage
	packets int64
}

// setElements reads `nft -j -n list set`: each element is a value or a
// concatenation, with its counter.
func setElements(out []byte) ([]element, int) {
	var listing struct {
		Nftables []struct {
			Set *struct {
				Size int               `json:"size"`
				Elem []json.RawMessage `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	if json.Unmarshal(out, &listing) != nil {
		return nil, 0
	}
	var elements []element
	size := 0
	for _, item := range listing.Nftables {
		if item.Set == nil {
			continue
		}
		size = item.Set.Size
		for _, raw := range item.Set.Elem {
			var wrapped struct {
				Elem *struct {
					Val     json.RawMessage `json:"val"`
					Counter *struct {
						Packets int64 `json:"packets"`
					} `json:"counter"`
				} `json:"elem"`
			}
			if json.Unmarshal(raw, &wrapped) != nil || wrapped.Elem == nil || wrapped.Elem.Counter == nil {
				continue
			}
			var concat struct {
				Concat []json.RawMessage `json:"concat"`
			}
			values := []json.RawMessage{wrapped.Elem.Val}
			if json.Unmarshal(wrapped.Elem.Val, &concat) == nil && concat.Concat != nil {
				values = concat.Concat
			}
			elements = append(elements, element{values: values, packets: wrapped.Elem.Counter.Packets})
		}
	}
	return elements, size
}

func intOf(raw json.RawMessage) int {
	value, _ := strconv.Atoi(string(raw))
	return value
}

// protocolName names the protocols the module reports by name; any other is
// its number.
func protocolName(raw json.RawMessage) string {
	switch number := intOf(raw); number {
	case 6:
		return "tcp"
	case 17:
		return "udp"
	case 1:
		return "icmp"
	case 58:
		return "icmpv6"
	default:
		return strconv.Itoa(number)
	}
}

type socket struct {
	protocol string
	port     int
	inode    string
	// onLink is a socket bound to a loopback or link-local address, which
	// nothing beyond this host or its own link can reach.
	onLink bool
}

// dhcpClientPorts are where DHCP clients listen for their server's answers;
// the rules let those in already, so they are not services to decide on.
var dhcpClientPorts = map[int]bool{68: true, 546: true}

// readSockets lists the listening TCP sockets and the unconnected UDP ones.
func readSockets(proc string) ([]socket, error) {
	var sockets []socket
	for _, source := range []struct{ file, protocol, state string }{
		{"net/tcp", "tcp", "0A"}, {"net/tcp6", "tcp", "0A"}, {"net/udp", "udp", "07"}, {"net/udp6", "udp", "07"},
	} {
		data, err := os.ReadFile(filepath.Join(proc, source.file))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return sockets, err
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != source.state {
				continue
			}
			address, port, ok := parseLocal(fields[1])
			if !ok {
				continue
			}
			sockets = append(sockets, socket{
				protocol: source.protocol, port: port, inode: fields[9],
				onLink: address.IsLoopback() || address.IsLinkLocalUnicast(),
			})
		}
	}
	return sockets, nil
}

// parseLocal reads "ADDRESS:PORT" in /proc/net form: the address is in host
// byte order per 32-bit word.
func parseLocal(text string) (netip.Addr, int, bool) {
	hexAddress, hexPort, found := strings.Cut(text, ":")
	raw, err := hex.DecodeString(hexAddress)
	port, perr := strconv.ParseUint(hexPort, 16, 16)
	if !found || err != nil || perr != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.Addr{}, 0, false
	}
	for word := 0; word < len(raw); word += 4 {
		slices.Reverse(raw[word : word+4])
	}
	address, _ := netip.AddrFromSlice(raw)
	return address.Unmap(), int(port), true
}

func listeningPorts(sockets []socket) []listening {
	seen := map[listening]bool{}
	result := []listening{}
	for _, s := range sockets {
		entry := listening{Protocol: s.protocol, Port: s.port}
		if s.onLink || s.port == 0 || seen[entry] || (s.protocol == "udp" && dhcpClientPorts[s.port]) {
			continue
		}
		seen[entry] = true
		result = append(result, entry)
	}
	slices.SortFunc(result, func(a, b listening) int {
		return cmp.Or(cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(a.Port, b.Port))
	})
	if len(result) > maxListening {
		result = result[:maxListening]
	}
	return result
}

// sshPorts are the TCP ports sshd itself listens on beyond loopback and link
// addresses; an sshd started by a socket unit is not recognized and gives none.
func sshPorts(proc string, sockets []socket) []int {
	owned := map[string]bool{}
	entries, _ := os.ReadDir(proc)
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(proc, entry.Name(), "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "sshd" {
			continue
		}
		fds, _ := os.ReadDir(filepath.Join(proc, entry.Name(), "fd"))
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(proc, entry.Name(), "fd", fd.Name()))
			if inode, found := strings.CutPrefix(link, "socket:["); err == nil && found {
				owned[strings.TrimSuffix(inode, "]")] = true
			}
		}
	}
	ports := []int{}
	for _, s := range sockets {
		if s.protocol == "tcp" && !s.onLink && owned[s.inode] && !slices.Contains(ports, s.port) && len(ports) < maxSSHPorts {
			ports = append(ports, s.port)
		}
	}
	slices.Sort(ports)
	return ports
}
