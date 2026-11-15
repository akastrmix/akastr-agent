// Package hostroute reads which interfaces carry this host's default routes.
package hostroute

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Defaults lists the interfaces of the IPv4 and IPv6 default routes, from
// every routing table the kernel lists under proc.
func Defaults(proc string) (v4, v6 []string, err error) {
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
