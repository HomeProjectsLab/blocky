//go:build !linux

package server

import "context"

// startSampler is a no-op off linux: gopsutil's per-core CPU % and disk-IO rates
// come from /proc, so the snapshot stays nil and GET /api/ui/system omits the
// system-usage fields (the UI header then hides itself). Keeps cross-compiles
// green, mirroring disk_free_linux.go / disk_free_other.go in querylog.
func (s *statsAPI) startSampler(context.Context) {}
