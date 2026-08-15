//go:build !linux

package querylog

import "errors"

// freeFraction is unsupported off linux, so the disk guardian no-ops there
// (blocky's appliance target is linux).
func freeFraction(string) (float64, error) {
	return 0, errors.New("disk-free check unsupported on this platform")
}
