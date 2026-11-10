package firewall

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The programs the module runs, each with fixed arguments. Rules reach nft on
// its standard input and never touch the disk.
const (
	NFTProgram = "/usr/sbin/nft"
	systemdRun = "/usr/bin/systemd-run"
	modprobe   = "/usr/sbin/modprobe"
	sysctl     = "/usr/sbin/sysctl"

	commandTimeout = 30 * time.Second
	probeTimeout   = 10 * time.Second
	// Each GiB of memory allows this many tracked connections (Cloud ADR 0108).
	conntrackPerGiB = 131072
)

// host is everything the module does outside its process.
type host interface {
	// installed reads the marker of the module's table; "" when it has none.
	installed(ctx context.Context) (string, error)
	// load checks a script and then applies it as one transaction.
	load(ctx context.Context, script string) error
	conntrackMax() (int64, bool)
	// raiseConntrack loads connection tracking and sets its limit.
	raiseConntrack(ctx context.Context, limit int64) error
	// probe opens a new connection to Cloud: name lookup, TCP and TLS.
	probe(ctx context.Context) error
	memory() (uint64, error)
}

type system struct {
	// nft is the command line that runs nft; tests run it in a namespace.
	nft      []string
	proc     string
	endpoint string
}

func newSystem(controlEndpoint string) *system {
	return &system{nft: []string{NFTProgram}, proc: "/proc", endpoint: controlEndpoint}
}

// HostCommands are the programs the installer provides for the module.
var HostCommands = []string{NFTProgram, modprobe, sysctl}

// Available refuses a configuration that turns the module on where nft is
// missing, so the node keeps its previous configuration and Cloud shows why.
func Available() error {
	if toolMissing() {
		return errors.New("module firewall needs " + NFTProgram + " (" + codeToolMissing + ")")
	}
	return nil
}

// toolMissing reports a node without nft; the installer provides it.
func toolMissing() bool {
	info, err := os.Stat(NFTProgram)
	return err != nil || info.IsDir() || info.Mode()&0o111 == 0
}

func (s *system) run(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, s.nft[0], append(clone(s.nft[1:]), args...)...)
	if stdin != "" {
		command.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("nft %s: %w: %s", args[0], err, firstLine(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func clone(s []string) []string { return append([]string(nil), s...) }

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}

func (s *system) tables(ctx context.Context) ([][2]string, error) {
	out, err := s.run(ctx, "", "-j", "list", "tables")
	if err != nil {
		return nil, err
	}
	var listing struct {
		Nftables []struct {
			Table *struct {
				Family string `json:"family"`
				Name   string `json:"name"`
			} `json:"table"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &listing); err != nil {
		return nil, err
	}
	var tables [][2]string
	for _, item := range listing.Nftables {
		if item.Table != nil {
			tables = append(tables, [2]string{item.Table.Family, item.Table.Name})
		}
	}
	return tables, nil
}

// nftablesLoaded reports whether the kernel has nf_tables. Without it there is
// no table, and asking nft would load it on a node whose firewall stays off.
func nftablesLoaded() bool {
	_, err := os.Stat("/sys/module/nf_tables")
	return err == nil
}

func (s *system) installed(ctx context.Context) (string, error) {
	if s.proc == "/proc" && !nftablesLoaded() {
		return "", nil
	}
	tables, err := s.tables(ctx)
	if err != nil {
		return "", err
	}
	present := false
	for _, t := range tables {
		present = present || t == [2]string{"inet", Table}
	}
	if !present {
		return "", nil
	}
	out, err := s.run(ctx, "", "-j", "list", "chain", "inet", Table, "input")
	if err != nil {
		return "", err
	}
	var listing struct {
		Nftables []struct {
			Rule *struct {
				Comment string `json:"comment"`
			} `json:"rule"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &listing); err != nil {
		return "", err
	}
	for _, item := range listing.Nftables {
		if item.Rule != nil {
			// A table without a readable marker is replaced like a stale one.
			if item.Rule.Comment == "" {
				return "unmarked", nil
			}
			return item.Rule.Comment, nil
		}
	}
	return "unmarked", nil
}

func (s *system) load(ctx context.Context, script string) error {
	if _, err := s.run(ctx, script, "-c", "-f", "-"); err != nil {
		return err
	}
	_, err := s.run(ctx, script, "-f", "-")
	return err
}

func (s *system) listSet(ctx context.Context, name string) ([]byte, error) {
	return s.run(ctx, "", "-j", "list", "set", "inet", Table, name)
}

func (s *system) conntrackMax() (int64, bool) {
	return readNumber(filepath.Join(s.proc, "sys/net/netfilter/nf_conntrack_max"))
}

func (s *system) conntrackCount() (int64, bool) {
	return readNumber(filepath.Join(s.proc, "sys/net/netfilter/nf_conntrack_count"))
}

func readNumber(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return value, err == nil
}

// raiseConntrack runs as a one-off root unit: the Agent's own unit cannot load
// modules or write kernel settings. The programs and arguments are fixed; only
// the number, computed here, varies.
func (s *system) raiseConntrack(ctx context.Context, limit int64) error {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	unit := []string{"--quiet", "--wait", "--collect", "--service-type=exec", "--description=akastr-firewall-conntrack"}
	for _, command := range [][]string{
		{modprobe, "nf_conntrack"},
		{sysctl, "-w", "net.netfilter.nf_conntrack_max=" + strconv.FormatInt(limit, 10)},
	} {
		if err := exec.CommandContext(ctx, systemdRun, append(clone(unit), command...)...).Run(); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(command[0]), err)
		}
	}
	return nil
}

func (s *system) probe(ctx context.Context) error {
	endpoint, err := url.Parse(s.endpoint)
	if err != nil || endpoint.Hostname() == "" {
		return errors.New("control endpoint is invalid")
	}
	port := endpoint.Port()
	if port == "" {
		port = "443"
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// A new dialer every time: nothing is reused, so the probe crosses the rules.
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{},
		Config:    &tls.Config{ServerName: endpoint.Hostname(), MinVersion: tls.VersionTLS12},
	}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(endpoint.Hostname(), port))
	if err != nil {
		return err
	}
	return connection.Close()
}

func (s *system) memory() (uint64, error) {
	file, err := os.Open(filepath.Join(s.proc, "meminfo"))
	if err != nil {
		return 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, err := strconv.ParseUint(fields[1], 10, 64)
			return kib * 1024, err
		}
	}
	return 0, errors.New("MemTotal missing")
}

// conntrackLimit is the tracked-connection limit for this much memory: whole
// GiB rounded up, so a "1 GB" machine counts as one.
func conntrackLimit(memory uint64) int64 {
	const gib = 1 << 30
	return int64((memory+gib-1)/gib) * conntrackPerGiB
}
