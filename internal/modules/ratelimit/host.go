package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TCProgram is the only program the module runs, with fixed argument shapes.
const TCProgram = "/usr/sbin/tc"

// HostCommands and HostPackages are what the installer provides for the module.
var (
	HostCommands = []string{TCProgram}
	HostPackages = []string{"iproute2"}
)

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

// links lists every network interface of the host. It asks the kernel rather
// than reading /sys/class/net, which also holds files such as bonding_masters.
func links() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ifaces))
	for _, i := range ifaces {
		names = append(names, i.Name)
	}
	return names, nil
}

func hasOwnFilter(out string) bool {
	return strings.Contains(out, fmt.Sprintf("pref %d ", prefIPv4)) || strings.Contains(out, fmt.Sprintf("pref %d ", prefIPv6))
}
