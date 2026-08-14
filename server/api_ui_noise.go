package server

import (
	"net/http"
	"strconv"
)

// Noise (decoy) dashboard endpoints. They mirror /api/ui/stats but read the
// reader's Decoy* methods, which scope to log_entries WHERE decoy = 1. Registered
// alongside the stats endpoints in registerStatsUIEndpoints.

func (s *statsAPI) noiseOverview(rw http.ResponseWriter, req *http.Request) {
	reader := s.readerOr503(rw)
	if reader == nil {
		return
	}

	from, to, err := parseTimeRange(req)
	if err != nil {
		badRequest(rw, err)

		return
	}

	overview, err := reader.DecoyOverview(from, to)
	if err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, overview)
}

func (s *statsAPI) noiseBuckets(rw http.ResponseWriter, req *http.Request) {
	reader := s.readerOr503(rw)
	if reader == nil {
		return
	}

	from, to, err := parseTimeRange(req)
	if err != nil {
		badRequest(rw, err)

		return
	}

	step := int64(3600) //nolint:mnd // one-hour default
	if v := req.URL.Query().Get("step"); v != "" {
		if step, err = strconv.ParseInt(v, 10, 64); err != nil {
			badRequest(rw, err)

			return
		}
	}

	buckets, err := reader.DecoyBuckets(from, to, step)
	if err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"buckets": buckets})
}

func (s *statsAPI) noiseTop(rw http.ResponseWriter, req *http.Request) {
	reader := s.readerOr503(rw)
	if reader == nil {
		return
	}

	from, to, err := parseTimeRange(req)
	if err != nil {
		badRequest(rw, err)

		return
	}

	items, err := reader.DecoyTopDomains(from, to, topN(req))
	if err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"items": items})
}

func (s *statsAPI) noiseSourceMix(rw http.ResponseWriter, req *http.Request) {
	reader := s.readerOr503(rw)
	if reader == nil {
		return
	}

	from, to, err := parseTimeRange(req)
	if err != nil {
		badRequest(rw, err)

		return
	}

	items, err := reader.DecoySourceMix(from, to)
	if err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"items": items})
}
