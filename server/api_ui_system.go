package server

import (
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/util"
)

// system reports process and storage health for the UI footer. Always answers
// 200: parts that are unavailable (no sqlite query log, DB file not yet
// created) report zero instead of failing the whole endpoint.
func (s *statsAPI) system(rw http.ResponseWriter, _ *http.Request) {
	var memStats runtime.MemStats

	runtime.ReadMemStats(&memStats)

	var dbConfigBytes int64
	if s.store != nil {
		dbConfigBytes = fileSize(s.store.DBPath())
	}

	var dbQuerylogBytes int64
	if s.qlCfg.Type == config.QueryLogTypeSqlite {
		dbQuerylogBytes = fileSize(s.qlCfg.Target.Reveal())
	}

	var queriesTotal int64
	if reader, err := s.getReader(); err == nil {
		queriesTotal, _ = reader.TotalQueries()
	}

	out := map[string]any{
		"version":         util.Version,
		"buildTime":       util.BuildTime,
		"uptimeSeconds":   int64(time.Since(s.start).Seconds()),
		"goroutines":      runtime.NumGoroutine(),
		"heapAllocBytes":  memStats.HeapAlloc,
		"dbConfigBytes":   dbConfigBytes,
		"dbQuerylogBytes": dbQuerylogBytes,
		"queriesTotal":    queriesTotal,
	}

	// Merge the latest system-usage sample when the sampler has published one
	// (linux only; nil before the first tick / on other platforms). Absent fields
	// are the degrade contract: the UI header stays hidden until they appear.
	if snap := s.sysUsage.Load(); snap != nil {
		out["cpuPerCore"] = snap.CPUPerCore
		out["cpuTotal"] = snap.CPUTotal
		out["memUsed"] = snap.MemUsed
		out["memTotal"] = snap.MemTotal
		out["diskUsed"] = snap.DiskUsed
		out["diskTotal"] = snap.DiskTotal
		out["diskReadBps"] = snap.DiskReadBps
		out["diskWriteBps"] = snap.DiskWriteBps
	}

	// Live query rate (real + decoy) over rolling windows, from the SSE hub's
	// per-second counter — for the UI's QPS readout next to the CPU strip.
	if s.hub != nil {
		out["qps10s"] = s.hub.QPS(10 * time.Second)
		out["qps1m"] = s.hub.QPS(time.Minute)
		out["qps5m"] = s.hub.QPS(5 * time.Minute)
		out["qps10m"] = s.hub.QPS(10 * time.Minute)
		out["qps1h"] = s.hub.QPS(time.Hour)
	}

	writeJSON(rw, http.StatusOK, out)
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}

	return info.Size()
}
