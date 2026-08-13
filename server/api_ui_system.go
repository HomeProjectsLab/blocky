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

	writeJSON(rw, http.StatusOK, map[string]any{
		"version":         util.Version,
		"buildTime":       util.BuildTime,
		"uptimeSeconds":   int64(time.Since(s.start).Seconds()),
		"goroutines":      runtime.NumGoroutine(),
		"heapAllocBytes":  memStats.HeapAlloc,
		"dbConfigBytes":   dbConfigBytes,
		"dbQuerylogBytes": dbQuerylogBytes,
		"queriesTotal":    queriesTotal,
	})
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}

	return info.Size()
}
