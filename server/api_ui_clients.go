package server

import (
	"encoding/json"
	"net/http"

	"github.com/0xERR0R/blocky/config"

	"github.com/go-chi/chi/v5"
)

// Clients + privacy UI endpoints. Registered alongside the other stats
// endpoints (they share the lazy sqlite reader and the config store).
//
// Contract:
//
//	GET  /api/ui/clients            -> {"clients":[{name,queries,blocked,lastSeen}]}
//	GET  /api/ui/clients/{name}     -> ClientDetail (history, transports,
//	                                   fingerprints[], topDomains) — see querylog.Reader
//	GET  /api/ui/privacy            -> the privacy config block (see privacyJSON)
//	PUT  /api/ui/privacy            -> apply a new privacy config block, 204 on success
//
// Client names are the aggregated "; "-joined client_name; the {name} path
// param is that exact string (URL-encoded by the browser).

func (s *statsAPI) clients(rw http.ResponseWriter, req *http.Request) {
	reader := s.readerOr503(rw)
	if reader == nil {
		return
	}

	from, to, err := parseTimeRange(req)
	if err != nil {
		badRequest(rw, err)

		return
	}

	list, err := reader.ClientList(from, to)
	if err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"clients": list})
}

func (s *statsAPI) clientDetail(rw http.ResponseWriter, req *http.Request) {
	reader := s.readerOr503(rw)
	if reader == nil {
		return
	}

	from, to, err := parseTimeRange(req)
	if err != nil {
		badRequest(rw, err)

		return
	}

	detail, err := reader.ClientDetail(chi.URLParam(req, "name"), from, to)
	if err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, detail)
}

// privacyJSON is the wire shape of the privacy config: explicit json tags keep
// the UI contract stable regardless of the Go field names (PercentPct etc.).
type privacyJSON struct {
	Decoy struct {
		Enable           bool    `json:"enable"`
		QueriesPerMinute float64 `json:"queriesPerMinute"`
		ReplayWeight     uint    `json:"replayWeight"`
		ListWeight       uint    `json:"listWeight"`
		ActiveHoursStart int     `json:"activeHoursStart"`
		ActiveHoursEnd   int     `json:"activeHoursEnd"`
		RefreshURL       string  `json:"refreshURL"`
	} `json:"decoy"`
	TTLJitter struct {
		Enable  bool `json:"enable"`
		Percent uint `json:"percent"`
	} `json:"ttlJitter"`
	EDNSPadding struct {
		Enable bool `json:"enable"`
	} `json:"ednsPadding"`
}

func privacyToJSON(p config.PrivacyConfig) privacyJSON {
	var j privacyJSON

	j.Decoy.Enable = p.Decoy.Enable
	j.Decoy.QueriesPerMinute = p.Decoy.QueriesPerMinute
	j.Decoy.ReplayWeight = p.Decoy.ReplayWeight
	j.Decoy.ListWeight = p.Decoy.ListWeight
	j.Decoy.ActiveHoursStart = p.Decoy.ActiveHoursStart
	j.Decoy.ActiveHoursEnd = p.Decoy.ActiveHoursEnd
	j.Decoy.RefreshURL = p.Decoy.RefreshURL
	j.TTLJitter.Enable = p.TTLJitter.Enable
	j.TTLJitter.Percent = p.TTLJitter.PercentPct
	j.EDNSPadding.Enable = p.EDNSPadding.Enable

	return j
}

func (j privacyJSON) toConfig() config.PrivacyConfig {
	var p config.PrivacyConfig

	p.Decoy.Enable = j.Decoy.Enable
	p.Decoy.QueriesPerMinute = j.Decoy.QueriesPerMinute
	p.Decoy.ReplayWeight = j.Decoy.ReplayWeight
	p.Decoy.ListWeight = j.Decoy.ListWeight
	p.Decoy.ActiveHoursStart = j.Decoy.ActiveHoursStart
	p.Decoy.ActiveHoursEnd = j.Decoy.ActiveHoursEnd
	p.Decoy.RefreshURL = j.Decoy.RefreshURL
	p.TTLJitter.Enable = j.TTLJitter.Enable
	p.TTLJitter.PercentPct = j.TTLJitter.Percent
	p.EDNSPadding.Enable = j.EDNSPadding.Enable

	return p
}

func (s *statsAPI) getPrivacy(rw http.ResponseWriter, _ *http.Request) {
	if s.store == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "config store not available"})

		return
	}

	p, err := s.store.GetPrivacy()
	if err != nil {
		internalError(rw, err)

		return
	}

	writeJSON(rw, http.StatusOK, privacyToJSON(p))
}

func (s *statsAPI) putPrivacy(rw http.ResponseWriter, req *http.Request) {
	if s.store == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "config store not available"})

		return
	}

	var body privacyJSON
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		badRequest(rw, err)

		return
	}

	// SetPrivacy validates the full candidate config before persisting.
	if err := s.store.SetPrivacy(body.toConfig()); err != nil {
		badRequest(rw, err)

		return
	}

	rw.WriteHeader(http.StatusNoContent)
}
