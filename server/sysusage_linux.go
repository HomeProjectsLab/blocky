//go:build linux

package server

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

const sysSampleInterval = 2 * time.Second

// partitionRe matches partition device names (sda1, vda1, mmcblk0p1, nvme0n1p1)
// so disk-IO totals sum whole disks only and don't double-count their partitions.
//
//nolint:gochecknoglobals // compiled-once regex
var partitionRe = regexp.MustCompile(`^(sd[a-z]+[0-9]+|vd[a-z]+[0-9]+|mmcblk[0-9]+p[0-9]+|nvme[0-9]+n[0-9]+p[0-9]+)$`)

// realDisk reports whether a /proc/diskstats device is a physical whole disk we
// should sum IO for — skips loop/ram/dm virtual devices and partitions.
//
// ponytail: sums all physical disks; on the single-disk appliance that's the one
// disk. Resolve the data dir's backing device if a multi-disk host ever appears.
func realDisk(name string) bool {
	if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "dm-") {
		return false
	}

	return !partitionRe.MatchString(name)
}

// startSampler runs a system-usage sampler until ctx is cancelled, publishing an
// atomic snapshot every sysSampleInterval. CPU% and disk IO are rates, so it
// holds the previous reading between ticks. Started ONCE on the server-lifetime
// ctx (never on a per-apply bundle ctx), so it survives config hot-swaps and
// stops only on real shutdown.
func (s *statsAPI) startSampler(ctx context.Context) {
	dir := "/"

	if s.store != nil {
		if p := s.store.DBPath(); p != "" {
			dir = filepath.Dir(p)
		}
	}

	go func() {
		ticker := time.NewTicker(sysSampleInterval)
		defer ticker.Stop()

		_, _ = cpu.Percent(0, true) // prime the per-core CPU delta
		prevIO, _ := disk.IOCounters()
		prevT := time.Now()

		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				snap := &sysSnapshot{}

				if per, err := cpu.Percent(0, true); err == nil {
					snap.CPUPerCore = per
					for _, p := range per {
						snap.CPUTotal += p
					}

					if n := len(per); n > 0 {
						snap.CPUTotal /= float64(n)
					}
				}

				if vm, err := mem.VirtualMemory(); err == nil {
					snap.MemUsed, snap.MemTotal = vm.Used, vm.Total
				}

				if du, err := disk.Usage(dir); err == nil {
					snap.DiskUsed, snap.DiskTotal = du.Used, du.Total
				}

				if io, err := disk.IOCounters(); err == nil {
					if dt := now.Sub(prevT).Seconds(); dt > 0 {
						var dr, dw uint64

						for name, c := range io {
							p, ok := prevIO[name]
							if !ok || !realDisk(name) {
								continue
							}
							// counters can reset (device re-add); guard the subtraction
							if c.ReadBytes >= p.ReadBytes {
								dr += c.ReadBytes - p.ReadBytes
							}

							if c.WriteBytes >= p.WriteBytes {
								dw += c.WriteBytes - p.WriteBytes
							}
						}

						snap.DiskReadBps = float64(dr) / dt
						snap.DiskWriteBps = float64(dw) / dt
					}

					prevIO, prevT = io, now
				}

				s.sysUsage.Store(snap)
			}
		}
	}()
}
