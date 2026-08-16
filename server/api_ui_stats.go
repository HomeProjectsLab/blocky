package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/configstore"
	"github.com/0xERR0R/blocky/querylog"

	"github.com/go-chi/chi/v5"
)

// registerStatsUIEndpoints mounts the dashboard stats / query explorer /
// live-stream / system API under /api/ui. All data comes from the sqlite
// query log; with any other query log target the endpoints respond 503.
// registerStatsUIEndpoints wires the stats/noise/queries API and returns the
// statsAPI so the server can Close its lazily-opened read-only reader on
// shutdown/rebuild (otherwise each apply leaks one sqlite RO connection).
func registerStatsUIEndpoints(
	ctx context.Context, router *chi.Mux, cfg *config.Config, hub *querylog.Hub,
	store *configstore.Store, classifier clientClassifier,
) *statsAPI {
	s := &statsAPI{qlCfg: cfg.QueryLog, hub: hub, store: store, classifier: classifier, start: time.Now()}

	// persistent system-usage sampler on the server-lifetime ctx (survives applies)
	s.startSampler(ctx)

	// background preheat of the default-window dashboard reads; stopped in Close()
	s.snap = newStatsSnapshot(s)

	router.Route("/api/ui/stats", func(r chi.Router) {
		r.Get("/overview", s.overview)
		r.Get("/buckets", s.buckets)
		r.Get("/top", s.top)
		r.Get("/latency", s.latency)
		r.Get("/categories", s.categories)
	})

	router.Route("/api/ui/noise", func(r chi.Router) {
		r.Get("/overview", s.noiseOverview)
		r.Get("/buckets", s.noiseBuckets)
		r.Get("/top", s.noiseTop)
		r.Get("/sourcemix", s.noiseSourceMix)
	})

	router.Get("/api/ui/queries", s.queries)
	router.Delete("/api/ui/queries", s.purgeQueries)
	router.Get("/api/ui/stream", s.stream)
	router.Get("/api/ui/logs", s.logsStream)        // live application log SSE
	router.Get("/api/ui/logs/recent", s.logsRecent) // ring snapshot, optional ?level=
	router.Get("/api/ui/system", s.system)
	router.Get("/api/ui/clients", s.clients)
	router.Get("/api/ui/clients/classes", s.clientClasses)
	router.Put("/api/ui/clients/classes/{client}", s.putClientClass)
	router.Put("/api/ui/clients/names/{client}", s.putClientName)
	router.Put("/api/ui/clients/persons/{client}", s.putClientPerson)
	router.Delete("/api/ui/clients/profiles", s.purgeProfiles)
	router.Get("/api/ui/people", s.people)
	router.Get("/api/ui/personas", s.personas)
	router.Get("/api/ui/clients/{name}", s.clientDetail)
	router.Get("/api/ui/privacy", s.getPrivacy)
	router.Put("/api/ui/privacy", s.putPrivacy)

	return s
}

// Close releases the lazily-opened read-only sqlite reader. Idempotent; safe on
// a statsAPI that never opened one.
func (s *statsAPI) Close() error {
	if s.snap != nil {
		s.snap.close()
	}

	return s.closeReader()
}

// closeReader releases the lazily-opened read-only reader WITHOUT stopping the
// snapshot goroutine, so purgeQueries can drop the handle and have the next
// refresh reopen against the emptied log. Idempotent.
func (s *statsAPI) closeReader() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.reader == nil {
		return nil
	}

	err := s.reader.Close()
	s.reader = nil

	return err
}

type statsAPI struct {
	qlCfg      config.QueryLog
	hub        *querylog.Hub
	store      *configstore.Store
	classifier clientClassifier // nil when no decoy store: class endpoints 503
	start      time.Time

	mu     sync.Mutex
	reader *querylog.Reader // constructed lazily: the writer must create the DB file first

	// snap preheats the default-window dashboard reads in the background so tab
	// loads serve a warm in-memory snapshot with no reader work. See snapshot.go.
	snap *statsSnapshot

	// client-class refresh throttle. RefreshClientClasses is a 7-day query-log
	// WINDOW scan — far too slow for the request path (seconds to tens of seconds
	// once the log is large) — so clientClasses kicks it in the background at most
	// once per interval and always serves the cheap cached client_class table.
	classMu         sync.Mutex
	classRefreshAt  time.Time
	classRefreshing bool

	// presence-profile refresh throttle (opt-in Profiling). Same shape as the
	// class throttle: RefreshClientProfiles is a full agg_hourly scan, off-path.
	profMu         sync.Mutex
	profRefreshAt  time.Time
	profRefreshing bool

	// short-TTL cache of store.GetPrivacy (a FULL config re-parse per call),
	// consulted several times per stats request. See getPrivacyCached.
	privMu  sync.Mutex
	privCfg config.PrivacyConfig
	privAt  time.Time
	privOK  bool

	// sysUsage holds the latest system-usage sample (per-core CPU / RAM / disk +
	// R/W), published by a persistent sampler on the server-lifetime ctx and
	// merged into GET /api/ui/system. nil until the first sample / on non-linux.
	sysUsage atomic.Pointer[sysSnapshot]
}

// getReader lazily opens the read-only sqlite handle. Errors are not cached:
// a failed open (e.g. DB file not written yet) is retried on the next request.
func (s *statsAPI) getReader() (*querylog.Reader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.reader != nil {
		return s.reader, nil
	}

	if s.qlCfg.Type != config.QueryLogTypeSqlite {
		return nil, errNotSqlite
	}

	reader, err := querylog.NewReader(s.qlCfg.Target.Reveal())
	if err != nil {
		return nil, err
	}

	s.reader = reader

	return reader, nil
}

type notSqliteError struct{}

func (notSqliteError) Error() string { return "query log not in sqlite mode" }

var errNotSqlite = notSqliteError{}

// readerOr503 resolves the reader, answering 503 itself on failure.
func (s *statsAPI) readerOr503(rw http.ResponseWriter) *querylog.Reader {
	reader, err := s.getReader()
	if err != nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})

		return nil
	}

	return reader
}

// parseTimeRange reads from/to (RFC3339) query params; defaults: to=now, from=now-24h.
func parseTimeRange(req *http.Request) (from, to time.Time, err error) {
	to = time.Now()

	if v := req.URL.Query().Get("to"); v != "" {
		if to, err = time.Parse(time.RFC3339, v); err != nil {
			return from, to, err
		}
	}

	// default from anchors on the PARSED to: with only to=<past> supplied,
	// anchoring on now would yield an inverted window and silent zero rows.
	from = to.Add(-24 * time.Hour)

	if v := req.URL.Query().Get("from"); v != "" {
		if from, err = time.Parse(time.RFC3339, v); err != nil {
			return from, to, err
		}
	}

	return from, to, nil
}

func badRequest(rw http.ResponseWriter, err error) {
	writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

func internalError(rw http.ResponseWriter, err error) {
	writeJSON(rw, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// purgeQueries deletes ALL logged queries and their hourly aggregate rollups.
// Config, blocklists, decoy corpus and client identity/class are untouched — only
// the query history and its stats are cleared.
func (s *statsAPI) purgeQueries(rw http.ResponseWriter, _ *http.Request) {
	if s.qlCfg.Type != config.QueryLogTypeSqlite {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "query log not in sqlite mode"})

		return
	}

	// Drop the cached read-only reader so subsequent reads reopen against the
	// emptied log instead of a handle whose page cache still holds deleted rows.
	// closeReader (not Close) keeps the snapshot goroutine alive so it repreheats
	// the now-empty window.
	_ = s.closeReader()

	if err := querylog.PurgeQueryLog(s.qlCfg.Target.Reveal()); err != nil {
		internalError(rw, err)

		return
	}

	// Drop the preheated snapshot too, or the dashboard keeps serving the purged
	// data for up to a full refresh interval after the user wiped it.
	if s.snap != nil {
		s.snap.invalidate()
	}

	rw.WriteHeader(http.StatusNoContent)
}

func (s *statsAPI) overview(rw http.ResponseWriter, req *http.Request) {
	reader := s.readerOr503(rw)
	if reader == nil {
		return
	}

	from, to, err := parseTimeRange(req)
	if err != nil {
		badRequest(rw, err)

		return
	}

	if ov, ok := s.snap.getOverview(from, to); ok {
		writeJSON(rw, http.StatusOK, ov)

		return
	}

	overview, err := reader.Overview(from, to)
	if err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, overview)
}

func (s *statsAPI) buckets(rw http.ResponseWriter, req *http.Request) {
	reader := s.readerOr503(rw)
	if reader == nil {
		return
	}

	from, to, err := parseTimeRange(req)
	if err != nil {
		badRequest(rw, err)

		return
	}

	step := int64(3600) //nolint:mnd // default: hourly, the stored granularity
	if v := req.URL.Query().Get("step"); v != "" {
		if step, err = strconv.ParseInt(v, 10, 64); err != nil {
			badRequest(rw, err)

			return
		}
	}

	if b, ok := s.snap.getBuckets(from, to, step); ok {
		writeJSON(rw, http.StatusOK, map[string]any{"buckets": b})

		return
	}

	buckets, err := reader.Buckets(from, to, step)
	if err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"buckets": buckets})
}

// topN reads the "n" query param and clamps it to [1, maxTopN]. A missing param
// yields the default; a garbage/negative/oversized value is clamped rather than
// rejected, so a hand-typed URL can't ask for a negative or unbounded result set.
const (
	defaultTopN = 10
	maxTopN     = 100
	// maxTopCols bounds the comma-separated col list: each token is one SQLite
	// GROUP BY, and only 5 columns exist — an unbounded list (150k tokens fit a
	// 1MB header) would starve the query-log writer for minutes.
	maxTopCols = 8
)

// dedupeCols removes duplicate tokens, preserving first-seen order.
func dedupeCols(cols []string) []string {
	seen := make(map[string]struct{}, len(cols))
	out := cols[:0]

	for _, c := range cols {
		if _, ok := seen[c]; !ok {
			seen[c] = struct{}{}
			out = append(out, c)
		}
	}

	return out
}

func topN(req *http.Request) int {
	n := defaultTopN
	if v, err := strconv.Atoi(req.URL.Query().Get("n")); err == nil {
		n = v
	}

	if n < 1 {
		return 1
	}

	if n > maxTopN {
		return maxTopN
	}

	return n
}

func (s *statsAPI) top(rw http.ResponseWriter, req *http.Request) {
	reader := s.readerOr503(rw)
	if reader == nil {
		return
	}

	from, to, err := parseTimeRange(req)
	if err != nil {
		badRequest(rw, err)

		return
	}

	n := topN(req)

	if cols, ok := s.snap.getTop(from, to, req.URL.Query().Get("col"), n); ok {
		writeJSON(rw, http.StatusOK, map[string]any{"columns": cols})

		return
	}

	// col may be a single column ({"items": [...]}) or a comma-separated list
	// ({"columns": {col: [...]}}). The dashboard batches its four top-N panels
	// into one request so a single page load stays under the browser's 6
	// connections-per-origin cap (one SSE stream already holds a slot).
	cols := dedupeCols(strings.Split(req.URL.Query().Get("col"), ","))
	if len(cols) > maxTopCols {
		badRequest(rw, fmt.Errorf("too many columns (max %d)", maxTopCols))

		return
	}

	if len(cols) > 1 {
		out := make(map[string][]querylog.TopItem, len(cols))

		for _, col := range cols {
			items, err := reader.Top(from, to, col, n)
			if err != nil {
				badRequest(rw, err)

				return
			}

			out[col] = items
		}

		writeJSON(rw, http.StatusOK, map[string]any{"columns": out})

		return
	}

	items, err := reader.Top(from, to, cols[0], n)
	if err != nil {
		badRequest(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"items": items})
}

// categories is the global activity-category timeline: per-category query
// totals over the window (eTLD+1 rule subset, blueprint R5). Opt-in — when
// profiling is off (privacy default) it returns an empty set, so the dashboard
// panel simply stays hidden without a separate feature probe.
func (s *statsAPI) categories(rw http.ResponseWriter, req *http.Request) {
	reader := s.readerOr503(rw)
	if reader == nil {
		return
	}

	enabled := false
	if s.store != nil {
		if cfg, err := s.getPrivacyCached(); err == nil {
			enabled = cfg.Profiling.Enable
		}
	}

	if !enabled {
		writeJSON(rw, http.StatusOK, map[string]any{"categories": []querylog.TopItem{}})

		return
	}

	from, to, err := parseTimeRange(req)
	if err != nil {
		badRequest(rw, err)

		return
	}

	if cats, ok := s.snap.getCategories(from, to); ok {
		writeJSON(rw, http.StatusOK, map[string]any{"categories": cats})

		return
	}

	items, err := reader.CategoryTotals(from, to)
	if err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"categories": items})
}

func (s *statsAPI) latency(rw http.ResponseWriter, req *http.Request) {
	reader := s.readerOr503(rw)
	if reader == nil {
		return
	}

	from, to, err := parseTimeRange(req)
	if err != nil {
		badRequest(rw, err)

		return
	}

	if p, ok := s.snap.getLatency(from, to); ok {
		writeJSON(rw, http.StatusOK, p)

		return
	}

	percentiles, err := reader.LatencyPercentiles(from, to)
	if err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, percentiles)
}

func (s *statsAPI) queries(rw http.ResponseWriter, req *http.Request) {
	reader := s.readerOr503(rw)
	if reader == nil {
		return
	}

	from, to, err := parseTimeRange(req)
	if err != nil {
		badRequest(rw, err)

		return
	}

	query := req.URL.Query()

	limit, _ := strconv.Atoi(query.Get("limit"))
	offset, _ := strconv.Atoi(query.Get("offset"))

	total, items, err := reader.Search(querylog.SearchFilter{
		Client:        query.Get("client"),
		Domain:        query.Get("domain"),
		Qtype:         query.Get("qtype"),
		Rtype:         query.Get("rtype"),
		From:          from,
		To:            to,
		Limit:         limit,
		Offset:        offset,
		IncludeDecoys: query.Get("decoys") == "true",
	})
	if err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"total": total, "items": items})
}
