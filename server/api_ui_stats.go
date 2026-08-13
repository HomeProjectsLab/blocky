package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/configstore"
	"github.com/0xERR0R/blocky/querylog"

	"github.com/go-chi/chi/v5"
)

// registerStatsUIEndpoints mounts the dashboard stats / query explorer /
// live-stream / system API under /api/ui. All data comes from the sqlite
// query log; with any other query log target the endpoints respond 503.
func registerStatsUIEndpoints(router *chi.Mux, cfg *config.Config, hub *querylog.Hub, store *configstore.Store) {
	s := &statsAPI{qlCfg: cfg.QueryLog, hub: hub, store: store, start: time.Now()}

	router.Route("/api/ui/stats", func(r chi.Router) {
		r.Get("/overview", s.overview)
		r.Get("/buckets", s.buckets)
		r.Get("/top", s.top)
		r.Get("/latency", s.latency)
	})

	router.Route("/api/ui/noise", func(r chi.Router) {
		r.Get("/overview", s.noiseOverview)
		r.Get("/buckets", s.noiseBuckets)
		r.Get("/top", s.noiseTop)
		r.Get("/sourcemix", s.noiseSourceMix)
	})

	router.Get("/api/ui/queries", s.queries)
	router.Get("/api/ui/stream", s.stream)
	router.Get("/api/ui/system", s.system)
	router.Get("/api/ui/clients", s.clients)
	router.Get("/api/ui/clients/{name}", s.clientDetail)
	router.Get("/api/ui/privacy", s.getPrivacy)
	router.Put("/api/ui/privacy", s.putPrivacy)
}

type statsAPI struct {
	qlCfg config.QueryLog
	hub   *querylog.Hub
	store *configstore.Store
	start time.Time

	mu     sync.Mutex
	reader *querylog.Reader // constructed lazily: the writer must create the DB file first
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

//nolint:gochecknoglobals // sentinel error
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
	from = to.Add(-24 * time.Hour) //nolint:mnd

	if v := req.URL.Query().Get("to"); v != "" {
		if to, err = time.Parse(time.RFC3339, v); err != nil {
			return from, to, err
		}
	}

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

	buckets, err := reader.Buckets(from, to, step)
	if err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"buckets": buckets})
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

	n := 10 //nolint:mnd // default top-n
	if v := req.URL.Query().Get("n"); v != "" {
		if n, err = strconv.Atoi(v); err != nil {
			badRequest(rw, err)

			return
		}
	}

	items, err := reader.Top(from, to, req.URL.Query().Get("col"), n)
	if err != nil {
		badRequest(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"items": items})
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
