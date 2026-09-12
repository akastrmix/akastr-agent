//go:build !linux

package autoupdate

import "os"

// Production maintenance is Linux-only. Other platforms compile pure logic tests.
func lockMaintenance(root string) (*os.File, error) { return os.Open(root) }
