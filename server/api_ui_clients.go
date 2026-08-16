package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/querylog"
)

// errShared rejects a per-device action (naming) on a NAT/shared client (R3).
var errShared = errors.New("client is a shared/NAT aggregate — a per-device name does not apply")

// clientClassRefreshInterval bounds how often the expensive 7-day client-class
// recompute runs off the request path.
const clientClassRefreshInterval = 5 * time.Minute

// clientClassifier is the slice of *querylog.DecoySource the clients UI needs to
// read/override per-client device classes. nil (no decoy store) => 503.
type clientClassifier interface {
	ListClientClasses() ([]querylog.ClientClassInfo, error)
	SetClientClassOverride(client, class string) error
	RefreshClientClasses() error
	ClientName(client string) (string, error)
	ClientNames() (map[string]string, error)
	SetClientName(client, name string) error
	// Person mapping (Phase 5, opt-in-within-opt-in — most sensitive). Mirrors
	// the name-override methods; gated behind the profiling toggle by the handler.
	ClientPerson(client string) (string, error)
	ClientPersons() (map[string]string, error)
	SetClientPerson(client, person string) error
	// Presence profiling (opt-in). RefreshClientProfiles is the heavy off-request
	// recompute; ClientProfile is a cheap PK read; PurgeProfiles wipes it all.
	RefreshClientProfiles() error
	ClientProfile(client string) (querylog.ClientProfileInfo, error)
	PurgeProfiles() error
}

// profileRefreshInterval bounds how often the presence recompute (full agg scan)
// runs off the request path. Mirrors clientClassRefreshInterval.
const profileRefreshInterval = 5 * time.Minute

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

	s.maybeRefreshClasses() // non-blocking, throttled; the list below is served from cache

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

// maybeRefreshClasses kicks a background client-class recompute at most once per
// clientClassRefreshInterval, and never blocks the caller. RefreshClientClasses
// is a 7-day query-log WINDOW scan that grows with log size (tens of seconds on
// a large log), so running it on the request path made GET /clients/classes hang
// and the device-class table never render. The cached client_class table is
// served immediately instead; the decoy engine also refreshes on its own cadence.
func (s *statsAPI) maybeRefreshClasses() {
	s.classMu.Lock()
	if s.classRefreshing || (!s.classRefreshAt.IsZero() && time.Since(s.classRefreshAt) < clientClassRefreshInterval) {
		s.classMu.Unlock()

		return
	}
	s.classRefreshing = true
	s.classMu.Unlock()

	go func() {
		if err := s.classifier.RefreshClientClasses(); err != nil {
			log.Log().Warnf("client-class refresh failed: %v", err)
		}

		s.classMu.Lock()
		s.classRefreshing = false
		s.classRefreshAt = time.Now()
		s.classMu.Unlock()
	}()
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

	if err := s.classifier.SetClientClassOverride(pathParam(req, "client"), body.Class); err != nil {
		badRequest(rw, err)

		return
	}

	rw.WriteHeader(http.StatusNoContent)
}

// allowPerDevice enforces the shared/NAT rejection gate (R3/P1) for a
// per-device write and reports whether the handler may proceed. It fails
// CLOSED: a garbage from/to falls back to the default 24h window instead of
// skipping the gate, and a ClientIsShared DB error rejects — this is the only
// enforcement point keeping a NAT aggregate from being named/mapped to a
// household member. The reader stays optional (no sqlite query log = no
// shared-client concept), so a missing reader still skips the gate.
func (s *statsAPI) allowPerDevice(rw http.ResponseWriter, req *http.Request, client string) bool {
	reader, err := s.getReader()
	if err != nil || reader == nil {
		return true
	}

	from, to, err := parseTimeRange(req)
	if err != nil {
		// bad range: gate on the default 24h window instead of skipping the gate
		to = time.Now()
		from = to.Add(-24 * time.Hour)
	}

	shared, err := reader.ClientIsShared(client, from, to)
	if err != nil {
		internalError(rw, err)

		return false
	}

	if shared {
		badRequest(rw, errShared)

		return false
	}

	return true
}

// putClientName sets (or clears, with a blank name) a client's manual display-name
// override. 503 when no classifier store; rejects a NAT/shared aggregate — a
// per-device name is meaningless for many devices behind one identity (R3).
func (s *statsAPI) putClientName(rw http.ResponseWriter, req *http.Request) {
	if s.classifier == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "device identity not available"})

		return
	}

	var body struct {
		Name string `json:"name"`
	}

	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		badRequest(rw, err)

		return
	}

	client := pathParam(req, "client")

	// R3: a shared/NAT identity must not carry a per-device name. Check via the
	// same enrich the list uses; the reader is optional so skip the gate if absent.
	if !s.allowPerDevice(rw, req, client) {
		return
	}

	if err := s.classifier.SetClientName(client, body.Name); err != nil {
		badRequest(rw, err)

		return
	}

	rw.WriteHeader(http.StatusNoContent)
}

// profilingOn reports whether the opt-in profiling toggle is enabled. Person
// mapping is the most-sensitive sub-feature and rides the same opt-in gate: OFF
// by default, so nothing here profiles a household member until the operator
// explicitly turns profiling on. (A dedicated Profiling.Person sub-toggle would
// live in config/privacy.go + privacy.js — Phase 3 files, out of this phase.)
func (s *statsAPI) profilingOn() bool {
	if s.store == nil {
		return false
	}

	cfg, err := s.store.GetPrivacy()

	return err == nil && cfg.Profiling.Enable
}

// putClientPerson maps a client to a named household member (or clears it with a
// blank person). This is the most sensitive layer: gated behind the profiling
// opt-in (503 when off), and — like naming — rejected for a NAT/shared aggregate,
// where the mapped unit would be a shared IP, not a person (blueprint P1/R3).
func (s *statsAPI) putClientPerson(rw http.ResponseWriter, req *http.Request) {
	if s.classifier == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "device identity not available"})

		return
	}

	if !s.profilingOn() {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "profiling is off — enable it in Privacy first"})

		return
	}

	var body struct {
		Person string `json:"person"`
	}

	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		badRequest(rw, err)

		return
	}

	client := pathParam(req, "client")

	// P1: a shared/NAT identity must not carry a per-person mapping. Same enrich
	// gate as naming; the reader is optional so skip the gate if absent.
	if !s.allowPerDevice(rw, req, client) {
		return
	}

	if err := s.classifier.SetClientPerson(client, body.Person); err != nil {
		badRequest(rw, err)

		return
	}

	rw.WriteHeader(http.StatusNoContent)
}

// personClient is one device attributed to a person: its windowed activity, plus
// a Shared flag when it is a NAT aggregate (P1 — the operator should un-map it,
// so it is still listed, just flagged).
type personClient struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Queries     int64  `json:"queries"`
	Blocked     int64  `json:"blocked"`
	Shared      bool   `json:"shared,omitempty"`
}

// personFootprint is one household member's rolled-up activity: the sum over
// every device mapped to them, plus those devices.
type personFootprint struct {
	Person  string         `json:"person"`
	Queries int64          `json:"queries"`
	Blocked int64          `json:"blocked"`
	Clients []personClient `json:"clients"`
}

// people rolls up per-client activity (summed from agg_hourly by ClientList) into
// per-person footprints — Phase 5, the most-sensitive layer. It profiles NAMED
// household members who did not consent, so it is gated behind the profiling
// opt-in and returns a bare {enabled:false} until profiling is turned on. The
// arithmetic is a plain Go rollup; the reliability caveats (NAT, rename) are
// carried as flags/UI copy, not silently absorbed.
func (s *statsAPI) people(rw http.ResponseWriter, req *http.Request) {
	if s.classifier == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "device identity not available"})

		return
	}

	if !s.profilingOn() {
		writeJSON(rw, http.StatusOK, map[string]any{"enabled": false})

		return
	}

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

	persons, err := s.classifier.ClientPersons() // client_name -> person
	if err != nil {
		internalError(rw, err)

		return
	}

	names, _ := s.classifier.ClientNames() // best-effort display-name overlay

	byPerson := map[string]*personFootprint{}
	seen := map[string]bool{}

	var unassigned []personClient

	getFP := func(person string) *personFootprint {
		fp := byPerson[person]
		if fp == nil {
			fp = &personFootprint{Person: person}
			byPerson[person] = fp
		}

		return fp
	}

	for _, c := range list {
		seen[c.Name] = true
		pc := personClient{
			Name: c.Name, DisplayName: names[c.Name],
			Queries: c.Queries, Blocked: c.Blocked,
			Shared: c.Shared || c.NatAggregate,
		}

		person := persons[c.Name]
		if person == "" {
			unassigned = append(unassigned, pc)

			continue
		}

		fp := getFP(person)
		fp.Queries += c.Queries
		fp.Blocked += c.Blocked
		fp.Clients = append(fp.Clients, pc)
	}

	// Mapped-but-idle devices (no traffic in the window) still appear under their
	// person with zero counts, so every mapping stays visible and un-mappable —
	// this is the purgeable, most-sensitive layer.
	for client, person := range persons {
		if seen[client] {
			continue
		}

		fp := getFP(person)
		fp.Clients = append(fp.Clients, personClient{Name: client, DisplayName: names[client]})
	}

	out := make([]*personFootprint, 0, len(byPerson))
	for _, fp := range byPerson {
		out = append(out, fp)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Queries > out[j].Queries })

	writeJSON(rw, http.StatusOK, map[string]any{
		"enabled":    true,
		"people":     out,
		"unassigned": unassigned,
	})
}

// Clients + privacy UI endpoints. Registered alongside the other stats
// endpoints (they share the lazy sqlite reader and the config store).
//
// Contract:
//
//	GET  /api/ui/clients            -> {"clients":[{name,queries,blocked,lastSeen,
//	                                   ips[],natAggregate,fpCount,os,vendor[],model[],
//	                                   apps[],deviceGuess,shared,sharedLabel}]}
//	GET  /api/ui/clients/{name}     -> ClientDetail (history, transports,
//	                                   fingerprints[], topDomains, plus the same
//	                                   identity facets) — see querylog.Reader
//
// Identity facets: os is a single conf-ranked guess; vendor/model/apps are
// med+confidence chip sets. A NAT/shared client (natAggregate) has all facets
// blanked and carries shared=true + sharedLabel "shared / N devices" instead.
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

	// Layer stored display-name overrides over auto-recognition (one query for
	// the whole page). Best-effort: a store error must not blank the client list.
	if s.classifier != nil {
		if names, nerr := s.classifier.ClientNames(); nerr == nil {
			for i := range list {
				if n := names[list[i].Name]; n != "" {
					list[i].DisplayName = n
				}
			}
		}
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

	detail, err := reader.ClientDetail(pathParam(req, "name"), from, to)
	if err != nil {
		internalError(rw, err)

		return
	}

	// Layer the stored display-name override (best-effort; PK lookup).
	if s.classifier != nil {
		if n, nerr := s.classifier.ClientName(detail.Name); nerr == nil {
			detail.DisplayName = n
		}
	}

	// Opt-in profiling: presence histogram (localized to the configured TZ) and
	// activity categories are both layered best-effort — disabled/missing => omitted.
	writeJSON(rw, http.StatusOK, clientDetailJSON{
		ClientDetail: detail,
		Presence:     s.presenceFor(detail.Name, detail.Shared),
		Categories:   s.categoriesFor(reader, detail.Name, from, to, detail.Shared),
	})
}

// clientDetailJSON wraps ClientDetail with the opt-in profiling extras. Both
// extras are omitted when off, so a non-profiling box sees the bare detail.
type clientDetailJSON struct {
	*querylog.ClientDetail
	Presence   *presenceJSON `json:"presence,omitempty"`
	Categories []string      `json:"categories,omitempty"`
}

// categoriesFor returns the client's activity categories, or nil when profiling
// is disabled (privacy default OFF), config is unavailable, or the client is a
// NAT/shared aggregate (R3 — categories of "many devices" are meaningless). No
// classifier needed: categories read log_entries directly through the reader.
func (s *statsAPI) categoriesFor(reader *querylog.Reader, name string, from, to time.Time, shared bool) []string {
	if s.store == nil || shared {
		return nil
	}

	cfg, err := s.store.GetPrivacy()
	if err != nil || !cfg.Profiling.Enable {
		return nil
	}

	cats, err := reader.ClientCategories(name, from, to)
	if err != nil {
		return nil
	}

	return cats
}

// presenceJSON is the localized presence histogram surfaced in the drill-down:
// queries per LOCAL hour-of-day (0..23) plus the zone used and last refresh.
type presenceJSON struct {
	HourLocal [24]int `json:"hourLocal"`
	TZ        string  `json:"tz"`
	UpdatedAt string  `json:"updatedAt,omitempty"`
}

// presenceFor returns the localized presence for a client, or nil when profiling
// is disabled, the store/config is unavailable, the client is a NAT/shared
// aggregate (R3 — presence of "many devices" is meaningless), or there is no
// profile row yet. It also kicks the throttled off-request recompute.
func (s *statsAPI) presenceFor(name string, shared bool) *presenceJSON {
	if s.classifier == nil || s.store == nil || shared {
		return nil
	}

	cfg, err := s.store.GetPrivacy()
	if err != nil || !cfg.Profiling.Enable {
		return nil
	}

	s.maybeRefreshProfiles()

	prof, err := s.classifier.ClientProfile(name)
	if err != nil {
		return nil
	}

	hist, tz := localizeHourHist(prof.HourHistUTC, cfg.Profiling.TZ)

	j := &presenceJSON{HourLocal: hist, TZ: tz}
	if !prof.UpdatedAt.IsZero() {
		j.UpdatedAt = prof.UpdatedAt.Format(time.RFC3339)
	}

	return j
}

// localizeHourHist rotates a UTC hour-of-day histogram into the configured local
// zone and returns the zone name used ("UTC" when tz is empty/invalid).
//
// ponytail: whole-hour rotation by the zone's CURRENT offset. This drops the
// :30/:45 fraction of India/Nepal-style zones and ignores the DST seam (a
// half-year of data crosses a 1h shift). The buckets are hour-granular, so a
// sub-hour-correct answer isn't representable without re-bucketing raw logs —
// upgrade path is a per-hour re-bucket in RefreshClientProfiles if it matters.
func localizeHourHist(utc [24]int, tz string) ([24]int, string) {
	loc, name := time.UTC, "UTC"
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc, name = l, tz
		}
	}

	_, offsetSec := time.Now().In(loc).Zone()
	shift := offsetSec / 3600

	var out [24]int
	for h := 0; h < 24; h++ {
		out[((h+shift)%24+24)%24] = utc[h]
	}

	return out, name
}

// maybeRefreshProfiles kicks a background presence recompute at most once per
// profileRefreshInterval, never blocking the caller (mirrors maybeRefreshClasses).
func (s *statsAPI) maybeRefreshProfiles() {
	s.profMu.Lock()
	if s.profRefreshing || (!s.profRefreshAt.IsZero() && time.Since(s.profRefreshAt) < profileRefreshInterval) {
		s.profMu.Unlock()

		return
	}
	s.profRefreshing = true
	s.profMu.Unlock()

	go func() {
		if err := s.classifier.RefreshClientProfiles(); err != nil {
			log.Log().Warnf("client-profile refresh failed: %v", err)
		}

		s.profMu.Lock()
		s.profRefreshing = false
		s.profRefreshAt = time.Now()
		s.profMu.Unlock()
	}()
}

// purgeProfiles wipes all precomputed presence data (the profiling purge button).
func (s *statsAPI) purgeProfiles(rw http.ResponseWriter, _ *http.Request) {
	if s.classifier == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "profiling not available"})

		return
	}

	if err := s.classifier.PurgeProfiles(); err != nil {
		internalError(rw, err)

		return
	}

	rw.WriteHeader(http.StatusNoContent)
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
		CohortJitterMs     uint `json:"cohortJitterMs"`
		CohortCompanionPct uint `json:"cohortCompanionPct"`
	} `json:"decoy"`
	// DeviceClass is DecoyConfig.DeviceClass, surfaced at the top level of the wire
	// shape so the flat privacy.js panel renderer can bind it like the other sections.
	DeviceClass struct {
		Enable            bool     `json:"enable"`
		VendorTelemetry   bool     `json:"vendorTelemetry"`
		VendorFamilies    []string `json:"vendorFamilies"`
		PhantomDevicesPct uint     `json:"phantomDevicesPct"`
	} `json:"deviceClass"`
	// Profiling is the opt-in, default-OFF presence analysis (local-only).
	Profiling struct {
		Enable bool   `json:"enable"`
		TZ     string `json:"tz"`
	} `json:"profiling"`
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
	j.Decoy.CohortJitterMs = p.Decoy.CohortJitterMs
	j.Decoy.CohortCompanionPct = p.Decoy.CohortCompanionPct
	j.DeviceClass.Enable = p.Decoy.DeviceClass.Enable
	j.DeviceClass.VendorTelemetry = p.Decoy.DeviceClass.VendorTelemetry
	j.DeviceClass.VendorFamilies = p.Decoy.DeviceClass.VendorFamilies
	j.DeviceClass.PhantomDevicesPct = p.Decoy.DeviceClass.PhantomDevicesPct
	j.Profiling.Enable = p.Profiling.Enable
	j.Profiling.TZ = p.Profiling.TZ
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
	p.Decoy.CohortJitterMs = j.Decoy.CohortJitterMs
	p.Decoy.CohortCompanionPct = j.Decoy.CohortCompanionPct
	p.Decoy.DeviceClass.Enable = j.DeviceClass.Enable
	p.Decoy.DeviceClass.VendorTelemetry = j.DeviceClass.VendorTelemetry
	p.Decoy.DeviceClass.VendorFamilies = j.DeviceClass.VendorFamilies
	p.Decoy.DeviceClass.PhantomDevicesPct = j.DeviceClass.PhantomDevicesPct
	p.Profiling.Enable = j.Profiling.Enable
	p.Profiling.TZ = j.Profiling.TZ
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
