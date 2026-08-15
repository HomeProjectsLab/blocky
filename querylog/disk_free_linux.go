//go:build linux

package querylog

import "syscall"

// freeFraction returns the fraction (0..1) of the filesystem containing dir
// that is available for unprivileged writes.
func freeFraction(dir string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}

	if st.Blocks == 0 {
		return 1, nil
	}

	return float64(st.Bavail) / float64(st.Blocks), nil
}
