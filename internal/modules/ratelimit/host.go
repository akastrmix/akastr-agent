package ratelimit

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TCProgram is the only program the module runs, with fixed argument shapes.
const TCProgram = "/usr/sbin/tc"

// runner runs tc with args, stdin as its input when not empty.
type runner interface {
	run(ctx context.Context, stdin string, args ...string) (string, error)
}

type execTC struct{}

// run keeps tc's output out of errors: batch lines name client addresses.
func (execTC) run(ctx context.Context, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, TCProgram, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tc %s failed: %w", args[0], err)
	}
	return out.String(), nil
}

var ownIndex = regexp.MustCompile(`police 0x([0-9a-f]+)`)

// cleanup removes every rule the module could have added on any interface,
// whichever process added it. It deletes the clsact hook only when nothing
// else is left on it.
func cleanup(ctx context.Context, tc runner, devs []string) error {
	var failures []string
	for _, dev := range devs {
		out, err := tc.run(ctx, "", "qdisc", "show", "dev", dev)
		if err != nil {
			failures = append(failures, dev)
			continue
		}
		if !strings.Contains(out, "clsact") {
			continue
		}
		for _, p := range []int{prefIPv4, prefIPv6} {
			// Fails harmlessly when the priority has no filters.
			_, _ = tc.run(ctx, "", "filter", "del", "dev", dev, "egress", "pref", strconv.Itoa(p))
		}
		egress, err1 := tc.run(ctx, "", "filter", "show", "dev", dev, "egress")
		ingress, err2 := tc.run(ctx, "", "filter", "show", "dev", dev, "ingress")
		if err1 != nil || err2 != nil || hasOwnFilter(egress) {
			failures = append(failures, dev)
			continue
		}
		if strings.TrimSpace(egress) == "" && strings.TrimSpace(ingress) == "" {
			if _, err := tc.run(ctx, "", "qdisc", "del", "dev", dev, "clsact"); err != nil {
				failures = append(failures, dev)
			}
		}
	}
	// A police action is released shortly after its last filter is deleted.
	for attempt := 0; ; attempt++ {
		out, err := tc.run(ctx, "", "actions", "ls", "action", "police")
		if err != nil {
			failures = append(failures, "actions")
			break
		}
		remaining := 0
		for _, m := range ownIndex.FindAllStringSubmatch(out, -1) {
			index, err := strconv.ParseUint(m[1], 16, 32)
			if err != nil || index <= actionBase || index > actionBase+maxUsers {
				continue
			}
			if _, err := tc.run(ctx, "", "actions", "del", "action", "police", "index", strconv.FormatUint(index, 10)); err != nil {
				remaining++
			}
		}
		if remaining == 0 {
			break
		}
		if attempt == 4 {
			failures = append(failures, "actions")
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if len(failures) > 0 {
		return errors.New("rate limit rules remain on " + strings.Join(failures, ", "))
	}
	return nil
}

// links lists every network interface of the host.
func links() ([]string, error) {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

func hasOwnFilter(out string) bool {
	return strings.Contains(out, fmt.Sprintf("pref %d ", prefIPv4)) || strings.Contains(out, fmt.Sprintf("pref %d ", prefIPv6))
}

// defaultRoutes lists the interfaces of the IPv4 and IPv6 default routes,
// which is where download to clients leaves.
func defaultRoutes(proc string) (v4, v6 []string, err error) {
	seen := map[string]bool{}
	err = scanLines(filepath.Join(proc, "net", "route"), func(f []string) {
		// Iface Destination Gateway Flags RefCnt Use Metric Mask ...
		if len(f) >= 8 && f[1] == "00000000" && f[7] == "00000000" && up(f[3]) && !seen["4"+f[0]] {
			seen["4"+f[0]] = true
			v4 = append(v4, f[0])
		}
	})
	if err != nil {
		return nil, nil, err
	}
	err = scanLines(filepath.Join(proc, "net", "ipv6_route"), func(f []string) {
		// dest destlen src srclen nexthop metric refcnt use flags iface
		if len(f) >= 10 && f[0] == strings.Repeat("0", 32) && f[1] == "00" && up(f[8]) && !reject(f[8]) && f[9] != "lo" && !seen["6"+f[9]] {
			seen["6"+f[9]] = true
			v6 = append(v6, f[9])
		}
	})
	if errors.Is(err, os.ErrNotExist) {
		// No IPv6 on this host.
		err = nil
	}
	return v4, v6, err
}

func up(flags string) bool {
	v, err := strconv.ParseUint(flags, 16, 32)
	return err == nil && v&0x1 != 0
}

func reject(flags string) bool {
	v, err := strconv.ParseUint(flags, 16, 32)
	return err != nil || v&0x200 != 0
}

func scanLines(path string, each func([]string)) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		each(strings.Fields(scanner.Text()))
	}
	return scanner.Err()
}
