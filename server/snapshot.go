package server

import (
	"strings"
	"sync"
	"time"

	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/querylog"
)

// Background-refreshed snapshot of the heavy default-window dashboard reads.
//
// Every tab (Dashboard, Clients, People, Privacy) first loads the SAME window —
// the last 24h ending now (parseTimeRange's default; dashboard.js opens on
// range "24h"). Each of those reads is an aggregation/scan over the multi-
// million-row log_entries table. Running them on the request path stalled the
// tab that triggered them AND, by saturating the Pi's SSD/CPU, every other tab.
//
// A single long-lived goroutine computes them ALL once at statsAPI startup
// (preheat), then recomputes on a ticker. A handler whose requested window is
// the default window serves the warm snapshot with ZERO reader work; any custom
// range falls through to the live reader path unchanged. This replaces N
// concurrent on-navigation scans with one gentle pass — a net DB-load
// reduction. Exactly one pass runs at a time: refresh is only ever called from
// the single run loop, so the ticker cannot overlap a pass in flight (busy
// ticks are dropped).

const (
	// snapshotRefresh is the recompute interval. One pass every ~45s.
	snapshotRefresh = 45 * time.Second

	// defaultWindow is the width every tab loads first (parseTimeRange default).
	defaultWindow = 24 * time.Hour

	// snapshotWindowSlop absorbs the to=now drift the browser sends on every
	// request: it stamps a fresh to=now (and from=now-24h) each call, so an exact
	// window equality never holds. A request matches the preheated default when
	// its to is within slop of now and its width is within slop of 24h. The
	// served data is itself up to snapshotRefresh stale — expected and harmless
	// at hour-bucket granularity.
	snapshotWindowSlop = 5 * time.Minute

	// defaultBucketStep is the bucket granularity the 24h dashboard requests
	// (STEPS["24h"] in dashboard.js). A different step computes live.
	defaultBucketStep = 900

	// defaultTopCols is the exact comma-joined column batch the dashboard sends
	// in its single /stats/top call (at defaultTopN). Only this batch is
	// preheated; any other column/n computes live.
	defaultTopCols = "domain,blocked,client,transport"
)

// isDefaultWindow reports whether [from,to] is the preheated default window.
func isDefaultWindow(from, to time.Time) bool {
	now := time.Now()
	if to.Before(now.Add(-snapshotWindowSlop)) || to.After(now.Add(snapshotWindowSlop)) {
		return false
	}

	dur := to.Sub(from)

	return dur > defaultWindow-snapshotWindowSlop && dur < defaultWindow+snapshotWindowSlop
}

type statsSnapshot struct {
	mu       sync.RWMutex
	ready    bool
	catReady bool // categories were computed this pass (profiling on)

	overview   *querylog.Overview
	buckets    []querylog.Bucket
	top        map[string][]querylog.TopItem
	categories []querylog.TopItem
	latency    *querylog.Percentiles
	clientList []querylog.ClientRow // shared read-only; callers that mutate must copy
	noise      *querylog.DecoyOverview

	stop     chan struct{}
	stopOnce sync.Once
}

// newStatsSnapshot starts the single preheat/refresh goroutine. Tie its
// lifetime to the statsAPI: close() stops it (called from statsAPI.Close).
func newStatsSnapshot(s *statsAPI) *statsSnapshot {
	snap := &statsSnapshot{stop: make(chan struct{})}
	go snap.run(s)

	return snap
}

func (snap *statsSnapshot) close() {
	snap.stopOnce.Do(func() { close(snap.stop) })
}

func (snap *statsSnapshot) run(s *statsAPI) {
	snap.refresh(s) // preheat immediately

	t := time.NewTicker(snapshotRefresh)
	defer t.Stop()

	for {
		select {
		case <-snap.stop:
			return
		case <-t.C:
			snap.refresh(s)
		}
	}
}

// refresh recomputes every default-window read, then publishes them atomically.
// DB work happens WITHOUT the lock held; the lock is taken only to swap in the
// finished results, so a request never sees a half-updated snapshot and never
// waits on a refresh. A reader/query error skips this pass; the next tick
// retries (this is also how a not-yet-openable reader at startup is handled).
func (snap *statsSnapshot) refresh(s *statsAPI) {
	// If close() already fired, don't reopen a reader the request path has
	// abandoned. ponytail: a rebuild-Close racing INSIDE the reads below can
	// still reopen once (leaks one RO conn until process exit); airtight would
	// need getReader to refuse post-close, but Close is also purgeQueries' drop-
	// and-reopen, so overloading it there is worse than this rare rebuild race.
	select {
	case <-snap.stop:
		return
	default:
	}

	reader, err := s.getReader()
	if err != nil {
		return // not openable yet (e.g. DB file not written) — ticker retries
	}

	to := time.Now()
	from := to.Add(-defaultWindow)

	overview, err := reader.Overview(from, to)
	if err != nil {
		log.Log().Warnf("snapshot overview: %v", err)

		return
	}

	buckets, err := reader.Buckets(from, to, defaultBucketStep)
	if err != nil {
		log.Log().Warnf("snapshot buckets: %v", err)

		return
	}

	top := make(map[string][]querylog.TopItem)
	for _, col := range strings.Split(defaultTopCols, ",") {
		items, err := reader.Top(from, to, col, defaultTopN)
		if err != nil {
			log.Log().Warnf("snapshot top %s: %v", col, err)

			return
		}

		top[col] = items
	}

	latency, err := reader.LatencyPercentiles(from, to)
	if err != nil {
		log.Log().Warnf("snapshot latency: %v", err)

		return
	}

	clientList, err := reader.ClientList(from, to)
	if err != nil {
		log.Log().Warnf("snapshot clients: %v", err)

		return
	}

	noise, err := reader.DecoyOverview(from, to)
	if err != nil {
		log.Log().Warnf("snapshot noise: %v", err)

		return
	}

	// categories is a log_entries scan only reached when profiling is ON (the
	// handler returns empty without a reader call otherwise). Skip it — the
	// common, default-OFF case — to keep each pass a smaller DB hit.
	var cats []querylog.TopItem

	catReady := false

	if s.profilingOn() {
		if c, cerr := reader.CategoryTotals(from, to); cerr == nil {
			cats, catReady = c, true
		}
	}

	snap.mu.Lock()
	snap.overview = overview
	snap.buckets = buckets
	snap.top = top
	snap.latency = latency
	snap.clientList = clientList
	snap.noise = noise
	snap.categories = cats
	snap.catReady = catReady
	snap.ready = true
	snap.mu.Unlock()
}

func (snap *statsSnapshot) getOverview(from, to time.Time) (*querylog.Overview, bool) {
	if !isDefaultWindow(from, to) {
		return nil, false
	}

	snap.mu.RLock()
	defer snap.mu.RUnlock()

	return snap.overview, snap.ready
}

func (snap *statsSnapshot) getBuckets(from, to time.Time, step int64) ([]querylog.Bucket, bool) {
	if step != defaultBucketStep || !isDefaultWindow(from, to) {
		return nil, false
	}

	snap.mu.RLock()
	defer snap.mu.RUnlock()

	return snap.buckets, snap.ready
}

func (snap *statsSnapshot) getTop(from, to time.Time, colParam string, n int) (map[string][]querylog.TopItem, bool) {
	if colParam != defaultTopCols || n != defaultTopN || !isDefaultWindow(from, to) {
		return nil, false
	}

	snap.mu.RLock()
	defer snap.mu.RUnlock()

	return snap.top, snap.ready
}

func (snap *statsSnapshot) getCategories(from, to time.Time) ([]querylog.TopItem, bool) {
	if !isDefaultWindow(from, to) {
		return nil, false
	}

	snap.mu.RLock()
	defer snap.mu.RUnlock()

	return snap.categories, snap.catReady
}

func (snap *statsSnapshot) getLatency(from, to time.Time) (*querylog.Percentiles, bool) {
	if !isDefaultWindow(from, to) {
		return nil, false
	}

	snap.mu.RLock()
	defer snap.mu.RUnlock()

	return snap.latency, snap.ready
}

// getClientList returns the shared default-window client list. It backs both
// /clients and /people. The slice is shared read-only — a caller that mutates
// rows (e.g. overlaying DisplayName) MUST copy first.
func (snap *statsSnapshot) getClientList(from, to time.Time) ([]querylog.ClientRow, bool) {
	if !isDefaultWindow(from, to) {
		return nil, false
	}

	snap.mu.RLock()
	defer snap.mu.RUnlock()

	return snap.clientList, snap.ready
}

func (snap *statsSnapshot) getNoise(from, to time.Time) (*querylog.DecoyOverview, bool) {
	if !isDefaultWindow(from, to) {
		return nil, false
	}

	snap.mu.RLock()
	defer snap.mu.RUnlock()

	return snap.noise, snap.ready
}
