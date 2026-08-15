package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/querylog"

	"github.com/go-chi/chi/v5"
)

// clientClassifier is the slice of *querylog.DecoySource the clients UI needs to
// read/override per-client device classes. nil (no decoy store) => 503.
type clientClassifier interface {
	ListClientClasses() ([]querylog.ClientClassInfo, error)
	SetClientClassOverride(client, class string) error
	RefreshClientClasses() error
}

type clientClassJSON struct {
	Client    string `json:"client"`
	Class     string `json:"class"`     // auto-detected
	Override  string `json:"override"`  // manual, "" if none
	Effective string `json:"effective"` // what the engine actually uses
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// clientClasses lists every client's device class (auto + override). A best-effort
// refresh runs first so the table reflects current traffic even if no engine timer
// has fired yet; the engine refreshes on its own cadence too.
func (s *statsAPI) clientClasses(rw http.ResponseWriter, _ *http.Request) {
	if s.classifier == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "device classification not available"})

		return
	}

	_ = s.classifier.RefreshClientClasses() // best-effort; stale list is still useful

	list, err := s.classifier.ListClientClasses()
	if err != nil {
		internalError(rw, err)

		return
	}

	out := make([]clientClassJSON, 0, len(list))
	for _, c := range list {
		j := clientClassJSON{Client: c.Client, Class: c.Class, Override: c.Override, Effective: c.Effective}
		if !c.UpdatedAt.IsZero() {
			j.UpdatedAt = c.UpdatedAt.Format(time.RFC3339)
		}

		out = append(out, j)
	}

	writeJSON(rw, http.StatusOK, map[string]any{"classes": out})
}

// putClientClass sets (or clears, with "" / "auto") a client's manual class override.
func (s *statsAPI) putClientClass(rw http.ResponseWriter, req *http.Request) {
	if s.classifier == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "device classification not available"})

		return
	}

	var body struct {
		Class string `json:"class"`
	}

	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		badRequest(rw, err)

		return
	}

	if err := s.classifier.SetClientClassOverride(chi.URLParam(req, "client"), body.Class); err != nil {
		badRequest(rw, err)

		return
	}

	rw.WriteHeader(http.StatusNoContent)
}

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
		PersonaProfile   string  `json:"personaProfile"`
		TargetQPMPeak    float64 `json:"targetQpmPeak"`
		TargetQPMTrough  float64 `json:"targetQpmTrough"`
		// Structural emission ("cohort") layer — shape the sequence/texture of decoys.
		CohortPct          uint `json:"cohortPct"`
		CompanionPct       uint `json:"companionPct"`
		ClusterPct         uint `json:"clusterPct"`
		StepPct            uint `json:"stepPct"`
		SessionCoherence   bool `json:"sessionCoherence"`
		RevisitCadence     bool `json:"revisitCadence"`
		PersonaCover       bool `json:"personaCover"`
		PersonaAttribution bool `json:"personaAttribution"`
	} `json:"decoy"`
	// DeviceClass is DecoyConfig.DeviceClass, surfaced at the top level of the wire
	// shape so the flat privacy.js panel renderer can bind it like the other sections.
	DeviceClass struct {
		Enable            bool     `json:"enable"`
		VendorTelemetry   bool     `json:"vendorTelemetry"`
		VendorFamilies    []string `json:"vendorFamilies"`
		PhantomDevicesPct uint     `json:"phantomDevicesPct"`
	} `json:"deviceClass"`
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
	j.Decoy.PersonaProfile = p.Decoy.PersonaProfile
	j.Decoy.TargetQPMPeak = p.Decoy.TargetQPMPeak
	j.Decoy.TargetQPMTrough = p.Decoy.TargetQPMTrough
	j.Decoy.CohortPct = p.Decoy.CohortPct
	j.Decoy.CompanionPct = p.Decoy.CompanionPct
	j.Decoy.ClusterPct = p.Decoy.ClusterPct
	j.Decoy.StepPct = p.Decoy.StepPct
	j.Decoy.SessionCoherence = p.Decoy.SessionCoherence
	j.Decoy.RevisitCadence = p.Decoy.RevisitCadence
	j.Decoy.PersonaCover = p.Decoy.PersonaCover
	j.Decoy.PersonaAttribution = p.Decoy.PersonaAttribution
	j.DeviceClass.Enable = p.Decoy.DeviceClass.Enable
	j.DeviceClass.VendorTelemetry = p.Decoy.DeviceClass.VendorTelemetry
	j.DeviceClass.VendorFamilies = p.Decoy.DeviceClass.VendorFamilies
	j.DeviceClass.PhantomDevicesPct = p.Decoy.DeviceClass.PhantomDevicesPct
	j.TTLJitter.Enable = p.TTLJitter.Enable
	j.TTLJitter.Percent = p.TTLJitter.PercentPct
	j.EDNSPadding.Enable = p.EDNSPadding.Enable

	return j
}

// applyTo overlays the wire fields onto an existing config (the current stored
// privacy config, with defaults applied), so any decoy knob NOT carried by the
// wire shape keeps its current value instead of being reset to a Go zero. This
// is what stops a Save on the (partial) Privacy panel from wiping the fields it
// doesn't render.
func (j privacyJSON) applyTo(p config.PrivacyConfig) config.PrivacyConfig {
	p.Decoy.Enable = j.Decoy.Enable
	p.Decoy.QueriesPerMinute = j.Decoy.QueriesPerMinute
	p.Decoy.ReplayWeight = j.Decoy.ReplayWeight
	p.Decoy.ListWeight = j.Decoy.ListWeight
	p.Decoy.ActiveHoursStart = j.Decoy.ActiveHoursStart
	p.Decoy.ActiveHoursEnd = j.Decoy.ActiveHoursEnd
	p.Decoy.RefreshURL = j.Decoy.RefreshURL
	p.Decoy.PersonaProfile = j.Decoy.PersonaProfile
	if p.Decoy.PersonaProfile == "" {
		p.Decoy.PersonaProfile = "auto" // empty = unset; matches the config default so partial bodies validate
	}
	p.Decoy.TargetQPMPeak = j.Decoy.TargetQPMPeak
	p.Decoy.TargetQPMTrough = j.Decoy.TargetQPMTrough
	p.Decoy.CohortPct = j.Decoy.CohortPct
	p.Decoy.CompanionPct = j.Decoy.CompanionPct
	p.Decoy.ClusterPct = j.Decoy.ClusterPct
	p.Decoy.StepPct = j.Decoy.StepPct
	p.Decoy.SessionCoherence = j.Decoy.SessionCoherence
	p.Decoy.RevisitCadence = j.Decoy.RevisitCadence
	p.Decoy.PersonaCover = j.Decoy.PersonaCover
	p.Decoy.PersonaAttribution = j.Decoy.PersonaAttribution
	p.Decoy.DeviceClass.Enable = j.DeviceClass.Enable
	p.Decoy.DeviceClass.VendorTelemetry = j.DeviceClass.VendorTelemetry
	p.Decoy.DeviceClass.VendorFamilies = j.DeviceClass.VendorFamilies
	p.Decoy.DeviceClass.PhantomDevicesPct = j.DeviceClass.PhantomDevicesPct
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

	// Overlay onto the CURRENT config (defaults applied) instead of a zero value,
	// so decoy knobs the wire shape doesn't carry are preserved across a save.
	cur, err := s.store.GetPrivacy()
	if err != nil {
		internalError(rw, err)

		return
	}

	// SetPrivacy validates the full candidate config before persisting.
	if err := s.store.SetPrivacy(body.applyTo(cur)); err != nil {
		badRequest(rw, err)

		return
	}

	rw.WriteHeader(http.StatusNoContent)
}
