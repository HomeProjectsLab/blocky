package querylog

import (
	"math/bits"
	"sort"
	"strings"
	"time"

	"github.com/0xERR0R/blocky/log"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Durable heuristics persistence (blueprint §2-4).
//
// Device intelligence — identity, recognition facets, temporal presence, service/
// category usage, and class — is folded into these fingerprint-keyed tables INSIDE
// the flush transaction (see upsertHeuristics, called from doDBWrite right after
// upsertNoiseCorpus). Every table here is durable BY CONSTRUCTION:
//
//   - it is NEVER added to purge.go's DELETE list, and
//   - it is populated on the write path, never re-derived from a log_entries scan,
//
// so each delta is already committed before any purge / retention / rotation can
// delete the raw row it came from. The class scorer (classScorerLoop) then reads
// only the tiny device_class_signal accumulator — never log_entries — so a class
// survives a full "clear all logs".
//
// KEYING: every table keys on the STABLE device key = fp_hash when present, else
// client_name (mirrors deviceKeyExpr in decoy_source.go). A DHCP/roam that changes
// the IP-fallback client_name but not the fingerprint keeps the same key.

const (
	// serviceCapPerDevice bounds service_usage — the one table with unbounded
	// distinct keys per device. Top-N by hits, evicted once per flush by
	// pruneServiceUsage. The app catalog is ~10 services, so this rarely trips.
	serviceCapPerDevice = 32
	// classScorerInterval is the class-scorer ticker cadence. Durable intelligence
	// must advance without a UI visit, so this is a real ticker, not a
	// request-triggered throttle (mirrors the former clientClassRefreshInterval).
	classScorerInterval = 5 * time.Minute
	// heuristicsStaleAfter bounds the heuristics tables: keys silent this long are
	// evicted. Without it the purge-immune tables grow forever — fp-less clients
	// key on churning IPv6 privacy addresses, one dead key set per rotation. Real
	// devices refresh last_seen on every flush and are never evicted.
	heuristicsStaleAfter = 30 * 24 * time.Hour
	// classScoreRecency bounds the scorer's read: only signals active in this
	// window are re-scored (an idle accumulator can't change, and its class is
	// already persisted in device_class/client_class).
	classScoreRecency = 24 * time.Hour
)

// --- durable schema (GORM structs; composite-PK style of aggHourly) -----------

// deviceIdentity is the durable fp anchor: first/last seen + lifetime hit count.
// Stability span = last_seen − first_seen (no extra column). distinct_clients is
// rolled up from fp_binding by the scorer, not folded.
type deviceIdentity struct {
	FpHash          string    `gorm:"column:fp_hash;primaryKey"`
	FirstSeen       time.Time `gorm:"column:first_seen;not null"`
	LastSeen        time.Time `gorm:"column:last_seen;not null;index:idx_device_identity_last_seen"`
	Hits            int64     `gorm:"column:hits;not null;default:0"`
	DistinctClients int64     `gorm:"column:distinct_clients;not null;default:0"`
}

func (deviceIdentity) TableName() string { return "device_identity" }

// fpBinding is the many-to-many client_name ↔ fp_hash bridge: the durable
// replacement for the per-snapshot COUNT(DISTINCT fp_hash) NAT aggregate.
// FpCount(client) = COUNT(*) WHERE client_name=? (covered PK-prefix scan).
type fpBinding struct {
	ClientName string    `gorm:"column:client_name;primaryKey"`
	FpHash     string    `gorm:"column:fp_hash;primaryKey;index:idx_fp_binding_fp"`
	Hits       int64     `gorm:"column:hits;not null;default:0"`
	FirstSeen  time.Time `gorm:"column:first_seen;not null"`
	LastSeen   time.Time `gorm:"column:last_seen;not null"`
}

func (fpBinding) TableName() string { return "fp_binding" }

// deviceFacet is one recognized (facet,label) per device with confidence.
type deviceFacet struct {
	FpHash    string    `gorm:"column:fp_hash;primaryKey"`
	Facet     string    `gorm:"column:facet;primaryKey;index:idx_device_facet_label,priority:1"`
	Label     string    `gorm:"column:label;primaryKey;index:idx_device_facet_label,priority:2"`
	Conf      float64   `gorm:"column:conf;not null;default:0"`
	Hits      int64     `gorm:"column:hits;not null;default:0"`
	FirstSeen time.Time `gorm:"column:first_seen;not null"`
	LastSeen  time.Time `gorm:"column:last_seen;not null"`
}

func (deviceFacet) TableName() string { return "device_facet" }

// devicePresence is the dow×hour activity histogram, one row per active cell
// (≤168/fp). Row-grain (not a CSV blob) so the fleet planning heatmap
// "who's active Tue 20:00" is index-backed on idx_device_presence_dowhour.
type devicePresence struct {
	FpHash string `gorm:"column:fp_hash;primaryKey"`
	Dow    int    `gorm:"column:dow;primaryKey;index:idx_device_presence_dowhour,priority:1"`
	Hour   int    `gorm:"column:hour;primaryKey;index:idx_device_presence_dowhour,priority:2"`
	Cnt    int64  `gorm:"column:cnt;not null;default:0"`
}

func (devicePresence) TableName() string { return "device_presence" }

// serviceUsage is per-device app/service usage. Top-N capped per fp (the growth
// bomb). format/category are attributes of the (fp,service) row, not key columns.
type serviceUsage struct {
	FpHash    string    `gorm:"column:fp_hash;primaryKey"`
	Service   string    `gorm:"column:service;primaryKey;index:idx_service_usage_service"`
	Format    string    `gorm:"column:format;not null;default:''"`
	Category  string    `gorm:"column:category;not null;default:''"`
	Hits      int64     `gorm:"column:hits;not null;default:0"`
	FirstSeen time.Time `gorm:"column:first_seen;not null"`
	LastSeen  time.Time `gorm:"column:last_seen;not null"`
}

func (serviceUsage) TableName() string { return "service_usage" }

// categoryUsage is per-device category magnitude. Bounded by the fixed catRules
// set × fp — no cap needed. Replaces the per-client half of CategoryTotals.
type categoryUsage struct {
	FpHash    string    `gorm:"column:fp_hash;primaryKey"`
	Category  string    `gorm:"column:category;primaryKey;index:idx_category_usage_category"`
	Hits      int64     `gorm:"column:hits;not null;default:0"`
	FirstSeen time.Time `gorm:"column:first_seen;not null"`
	LastSeen  time.Time `gorm:"column:last_seen;not null"`
}

func (categoryUsage) TableName() string { return "category_usage" }

// deviceClass is the scored output — the fp-keyed twin of client_class, written
// ONLY by the scorer from the durable accumulator (never a log_entries scan).
type deviceClass struct {
	FpHash    string    `gorm:"column:fp_hash;primaryKey"`
	Class     string    `gorm:"column:class;not null;default:''"`
	Override  string    `gorm:"column:override;not null;default:''"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
}

func (deviceClass) TableName() string { return "device_class" }

// deviceClassSignal is the accumulator the scorer reads (mirrors classFeatures).
// The only order-dependent state is last_ts / last_domain + the Welford gap sums;
// everything else is a commutative counter. domains is synced with
// device_class_domainset (capped ≤ classIoTMaxDomains+1).
type deviceClassSignal struct {
	FpHash      string `gorm:"column:fp_hash;primaryKey"`
	N           int64  `gorm:"column:n;not null;default:0"`
	ServerHits  int64  `gorm:"column:server_hits;not null;default:0"`
	GameHits    int64  `gorm:"column:game_hits;not null;default:0"`
	CameraHits  int64  `gorm:"column:camera_hits;not null;default:0"`
	SpeakerHits int64  `gorm:"column:speaker_hits;not null;default:0"`
	PrinterHits int64  `gorm:"column:printer_hits;not null;default:0"`
	StreamHits  int64  `gorm:"column:stream_hits;not null;default:0"`
	PushHits    int64  `gorm:"column:push_hits;not null;default:0"`
	// +8 vendor-tell classes (AutoMigrate adds these columns to existing DBs).
	NASHits        int64      `gorm:"column:nas_hits;not null;default:0"`
	RouterHits     int64      `gorm:"column:router_hits;not null;default:0"`
	MediaHits      int64      `gorm:"column:media_hits;not null;default:0"`
	SmartTVHits    int64      `gorm:"column:smart_tv_hits;not null;default:0"`
	HubHits        int64      `gorm:"column:hub_hits;not null;default:0"`
	ThermostatHits int64      `gorm:"column:thermostat_hits;not null;default:0"`
	LightingHits   int64      `gorm:"column:lighting_hits;not null;default:0"`
	WearableHits   int64      `gorm:"column:wearable_hits;not null;default:0"`
	QtypeMask      int64      `gorm:"column:qtype_mask;not null;default:0"`
	Domains        int64      `gorm:"column:domains;not null;default:0"`
	LastTs         *time.Time `gorm:"column:last_ts"`
	LastDomain     string     `gorm:"column:last_domain;not null;default:''"`
	GapSum         float64    `gorm:"column:gap_sum;not null;default:0"`
	GapSqSum       float64    `gorm:"column:gap_sq_sum;not null;default:0"`
	GapN           int64      `gorm:"column:gap_n;not null;default:0"`
}

func (deviceClassSignal) TableName() string { return "device_class_signal" }

// toFeatures maps the accumulator to classFeatures so the scorer runs the exact
// same classify() as the retired log_entries scan.
func (a deviceClassSignal) toFeatures() classFeatures {
	var mean, mean2 float64
	if a.GapN > 0 {
		mean = a.GapSum / float64(a.GapN)
		mean2 = a.GapSqSum / float64(a.GapN)
	}

	return classFeatures{
		Client:         a.FpHash,
		N:              a.N,
		Domains:        a.Domains,
		Qtypes:         int64(bits.OnesCount64(uint64(a.QtypeMask))),
		ServerHits:     a.ServerHits,
		GameHits:       a.GameHits,
		CameraHits:     a.CameraHits,
		SpeakerHits:    a.SpeakerHits,
		PrinterHits:    a.PrinterHits,
		StreamHits:     a.StreamHits,
		PushHits:       a.PushHits,
		NASHits:        a.NASHits,
		RouterHits:     a.RouterHits,
		MediaHits:      a.MediaHits,
		SmartTVHits:    a.SmartTVHits,
		HubHits:        a.HubHits,
		ThermostatHits: a.ThermostatHits,
		LightingHits:   a.LightingHits,
		WearableHits:   a.WearableHits,
		MeanGap:        mean,
		MeanGap2:       mean2,
	}
}

// deviceClassDomainset is the bounded distinct-eTLD+1 set for the IoT test. Capped
// at classIoTMaxDomains+1 rows/fp: once >8, the IoT-domain test is decided forever,
// so folding stops inserting. Exact at the boundary, bounded memory.
type deviceClassDomainset struct {
	FpHash string `gorm:"column:fp_hash;primaryKey"`
	Etldp  string `gorm:"column:etldp;primaryKey"`
}

func (deviceClassDomainset) TableName() string { return "device_class_domainset" }

// personaLink maps an fp to a household person. Manual, write-through, never
// recomputed — schema only here; the write path is the existing person UI.
type personaLink struct {
	FpHash    string    `gorm:"column:fp_hash;primaryKey"`
	Person    string    `gorm:"column:person;not null;index:idx_persona_link_person"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
}

func (personaLink) TableName() string { return "persona_link" }

// heuristicsTables is the AutoMigrate set (structs + composite PKs + every index
// via gorm tags — idempotent CREATE ... IF NOT EXISTS semantics). Registered next
// to aggHourly/noiseCorpus in databaseMigration and NewDecoySource.
//
//nolint:gochecknoglobals // static migration list
var heuristicsTables = []any{
	&deviceIdentity{}, &fpBinding{}, &deviceFacet{}, &devicePresence{},
	&serviceUsage{}, &categoryUsage{}, &deviceClass{}, &deviceClassSignal{},
	&deviceClassDomainset{}, &personaLink{},
}

// --- match helpers (reuse the existing signal catalogs) -----------------------

// serverEtldpSet is serverEtldps as a lookup set (RefreshClientClasses did
// `etldp IN (...)`; the fold does a Go map lookup instead).
//
//nolint:gochecknoglobals // derived once from the static catalog
var serverEtldpSet = func() map[string]bool {
	m := make(map[string]bool, len(serverEtldps))
	for _, d := range serverEtldps {
		m[d] = true
	}

	return m
}()

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}

	return false
}

// qtypeBit maps a qtype string to one bit, so ORing bits then popcount yields the
// distinct-qtype count classFeatures needs (threshold ≤ 3). Common types get fixed
// bits; the rest fold into a small hashed range — exact enough at the threshold.
func qtypeBit(qt string) int64 {
	fixed := map[string]uint{
		"A": 0, "AAAA": 1, "CNAME": 2, "MX": 3, "NS": 4, "PTR": 5, "SOA": 6,
		"SRV": 7, "TXT": 8, "HTTPS": 9, "SVCB": 10, "NAPTR": 11, "CAA": 12,
		"DS": 13, "DNSKEY": 14, "TLSA": 15, "ANY": 16,
	}
	if b, ok := fixed[qt]; ok {
		return int64(1) << b
	}

	var h uint32 = 2166136261
	for i := 0; i < len(qt); i++ {
		h = (h ^ uint32(qt[i])) * 16777619
	}

	return int64(1) << (17 + h%40) // bits 17..56, disjoint from the fixed range
}

// heuristicsKey is the stable device key for a raw row: fp_hash, else client_name
// (mirrors deviceKeyExpr). Empty when both are empty — such a row is skipped.
func heuristicsKey(e *logEntry) string {
	if e.FpHash != "" {
		return e.FpHash
	}

	return e.ClientName
}

// --- the fold (blueprint §3.1) ------------------------------------------------

// clsAccum is the in-memory per-key class-signal accumulation for one batch,
// before it is folded against the stored cursor.
type clsAccum struct {
	n                                                    int64
	server, game, camera, speaker, printer, stream, push int64
	nas, router, media, smartTV, hub                     int64
	thermostat, lighting, wearable                       int64
	qtypeMask                                            int64
	tss                                                  []time.Time // for gap stats (sorted before use)
	etldps                                               []string    // first-seen order, for the capped domainset
	etldpSeen                                            map[string]bool
	lastDomain                                           string
	lastTs                                               time.Time
}

// upsertHeuristics folds a raw batch into every durable heuristics table inside
// tx. Called from doDBWrite in the SAME transaction as the raw insert + aggregates
// + noise corpus, so each delta commits before any purge can delete its source
// row. Decoys and key-less rows are skipped; blocked rows are kept (a device that
// only makes blocked queries is still present / classifiable — matches the former
// RefreshClientClasses `decoy = 0` scan).
func upsertHeuristics(tx *gorm.DB, entries []*logEntry) error {
	idents := map[string]*deviceIdentity{}
	binds := map[[2]string]*fpBinding{}
	facets := map[[3]string]*deviceFacet{}
	pres := map[[3]any]*devicePresence{}
	svcs := map[[2]string]*serviceUsage{}
	cats := map[[2]string]*categoryUsage{}
	cls := map[string]*clsAccum{}

	appRules := facetRules("app", confLow) // the "service" catalog is the app facet

	for _, e := range entries {
		if e.Decoy {
			continue
		}

		key := heuristicsKey(e)
		if key == "" {
			continue
		}

		ts := e.RequestTS
		utc := ts.UTC()
		qn := e.QuestionName

		// identity
		if id := idents[key]; id != nil {
			id.Hits++
			id.spanSeen(ts)
		} else {
			idents[key] = &deviceIdentity{FpHash: key, FirstSeen: ts, LastSeen: ts, Hits: 1}
		}

		// fp ↔ client binding (only meaningful when the row carries a client_name)
		if e.ClientName != "" {
			bk := [2]string{e.ClientName, key}
			if b := binds[bk]; b != nil {
				b.Hits++
				spanSeen(&b.FirstSeen, &b.LastSeen, ts)
			} else {
				binds[bk] = &fpBinding{ClientName: e.ClientName, FpHash: key, Hits: 1, FirstSeen: ts, LastSeen: ts}
			}
		}

		// presence (dow×hour cell)
		pk := [3]any{key, utc.Weekday(), utc.Hour()}
		if p := pres[pk]; p != nil {
			p.Cnt++
		} else {
			pres[pk] = &devicePresence{FpHash: key, Dow: int(utc.Weekday()), Hour: utc.Hour(), Cnt: 1}
		}

		// recognition facets (every matching sigRule)
		for _, r := range sigRules {
			if !strings.Contains(qn, r.match) {
				continue
			}

			fk := [3]string{key, r.facet, r.label}
			if f := facets[fk]; f != nil {
				f.Hits++
				f.Conf = maxF(f.Conf, float64(r.conf))
				spanSeen(&f.FirstSeen, &f.LastSeen, ts)
			} else {
				facets[fk] = &deviceFacet{
					FpHash: key, Facet: r.facet, Label: r.label,
					Conf: float64(r.conf), Hits: 1, FirstSeen: ts, LastSeen: ts,
				}
			}
		}

		// service usage (app-facet catalog), category attribute from catRulesHost
		for _, r := range appRules {
			if !strings.Contains(qn, r.match) {
				continue
			}

			sk := [2]string{key, r.label}
			if sv := svcs[sk]; sv != nil {
				sv.Hits++
				spanSeen(&sv.FirstSeen, &sv.LastSeen, ts)
			} else {
				svcs[sk] = &serviceUsage{
					FpHash: key, Service: r.label, Category: categoryFor(qn),
					Hits: 1, FirstSeen: ts, LastSeen: ts,
				}
			}
		}

		// category usage
		if cat := categoryFor(qn); cat != "" {
			ck := [2]string{key, cat}
			if c := cats[ck]; c != nil {
				c.Hits++
				spanSeen(&c.FirstSeen, &c.LastSeen, ts)
			} else {
				cats[ck] = &categoryUsage{FpHash: key, Category: cat, Hits: 1, FirstSeen: ts, LastSeen: ts}
			}
		}

		// class-signal accumulation
		foldClassSignal(cls, key, e, ts)
	}

	return applyHeuristics(tx, idents, binds, facets, pres, svcs, cats, cls)
}

// foldClassSignal accumulates one row into the per-key class signal.
func foldClassSignal(cls map[string]*clsAccum, key string, e *logEntry, ts time.Time) {
	a := cls[key]
	if a == nil {
		a = &clsAccum{etldpSeen: map[string]bool{}}
		cls[key] = a
	}

	a.n++
	a.qtypeMask |= qtypeBit(e.QuestionType)

	qn := e.QuestionName
	if serverEtldpSet[e.EffectiveTLDP] || containsAny(qn, serverNameLikes) {
		a.server++
	}

	if containsAny(qn, gameSignals) {
		a.game++
	}

	if containsAny(qn, cameraSignals) {
		a.camera++
	}

	if containsAny(qn, speakerSignals) {
		a.speaker++
	}

	if containsAny(qn, printerSignals) {
		a.printer++
	}

	if containsAny(qn, streamSignals) {
		a.stream++
	}

	if containsAny(qn, pushSignals) {
		a.push++
	}

	if containsAny(qn, nasSignals) {
		a.nas++
	}

	if containsAny(qn, routerSignals) {
		a.router++
	}

	if containsAny(qn, mediaSignals) {
		a.media++
	}

	if containsAny(qn, smartTVSignals) {
		a.smartTV++
	}

	if containsAny(qn, hubSignals) {
		a.hub++
	}

	if containsAny(qn, thermostatSignals) {
		a.thermostat++
	}

	if containsAny(qn, lightingSignals) {
		a.lighting++
	}

	if containsAny(qn, wearableSignals) {
		a.wearable++
	}

	a.tss = append(a.tss, ts)

	if etldp := e.EffectiveTLDP; etldp != "" {
		if !a.etldpSeen[etldp] {
			a.etldpSeen[etldp] = true
			a.etldps = append(a.etldps, etldp)
		}

		if ts.After(a.lastTs) || a.lastDomain == "" {
			a.lastTs = ts
			a.lastDomain = etldp
		}
	}
}

// applyHeuristics writes all deduped maps with one OnConflict upsert per table,
// verbatim the upsertNoiseCorpus / upsertAggregates shape.
func applyHeuristics(tx *gorm.DB,
	idents map[string]*deviceIdentity, binds map[[2]string]*fpBinding,
	facets map[[3]string]*deviceFacet, pres map[[3]any]*devicePresence,
	svcs map[[2]string]*serviceUsage, cats map[[2]string]*categoryUsage,
	cls map[string]*clsAccum,
) error {
	if len(idents) > 0 {
		rows := mapVals(idents)
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "fp_hash"}},
			DoUpdates: clause.Assignments(map[string]any{
				"hits":      gorm.Expr("hits + excluded.hits"),
				"last_seen": gorm.Expr("MAX(last_seen, excluded.last_seen)"),
			}),
		}).Create(&rows).Error; err != nil {
			return err
		}
	}

	if len(binds) > 0 {
		rows := mapVals(binds)
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "client_name"}, {Name: "fp_hash"}},
			DoUpdates: clause.Assignments(map[string]any{
				"hits":      gorm.Expr("hits + excluded.hits"),
				"last_seen": gorm.Expr("MAX(last_seen, excluded.last_seen)"),
			}),
		}).Create(&rows).Error; err != nil {
			return err
		}
	}

	if len(facets) > 0 {
		rows := mapVals(facets)
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "fp_hash"}, {Name: "facet"}, {Name: "label"}},
			DoUpdates: clause.Assignments(map[string]any{
				"hits":      gorm.Expr("hits + excluded.hits"),
				"conf":      gorm.Expr("MAX(conf, excluded.conf)"),
				"last_seen": gorm.Expr("MAX(last_seen, excluded.last_seen)"),
			}),
		}).Create(&rows).Error; err != nil {
			return err
		}
	}

	if len(pres) > 0 {
		rows := mapVals(pres)
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "fp_hash"}, {Name: "dow"}, {Name: "hour"}},
			DoUpdates: clause.Assignments(map[string]any{"cnt": gorm.Expr("cnt + excluded.cnt")}),
		}).Create(&rows).Error; err != nil {
			return err
		}
	}

	if len(svcs) > 0 {
		rows := mapVals(svcs)
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "fp_hash"}, {Name: "service"}},
			DoUpdates: clause.Assignments(map[string]any{
				"hits":      gorm.Expr("hits + excluded.hits"),
				"last_seen": gorm.Expr("MAX(last_seen, excluded.last_seen)"),
			}),
		}).Create(&rows).Error; err != nil {
			return err
		}
	}

	if len(cats) > 0 {
		rows := mapVals(cats)
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "fp_hash"}, {Name: "category"}},
			DoUpdates: clause.Assignments(map[string]any{
				"hits":      gorm.Expr("hits + excluded.hits"),
				"last_seen": gorm.Expr("MAX(last_seen, excluded.last_seen)"),
			}),
		}).Create(&rows).Error; err != nil {
			return err
		}
	}

	return applyClassSignal(tx, cls)
}

// applyClassSignal folds the class accumulators against the stored cursor: read
// each key's prior last_ts (gap origin) + domains (cap gate), insert new eTLD+1s
// into the capped domainset, then upsert the additive/OR/Welford deltas.
func applyClassSignal(tx *gorm.DB, cls map[string]*clsAccum) error {
	if len(cls) == 0 {
		return nil
	}

	keys := make([]string, 0, len(cls))
	for k := range cls {
		keys = append(keys, k)
	}

	// prior cursor state (empty for brand-new keys)
	type cursor struct {
		FpHash  string
		LastTs  *time.Time
		Domains int64
	}

	var prev []cursor
	if err := tx.Raw(
		"SELECT fp_hash, last_ts, domains FROM device_class_signal WHERE fp_hash IN ?", keys,
	).Scan(&prev).Error; err != nil {
		return err
	}

	priorTs := map[string]time.Time{}
	priorDomains := map[string]int64{}

	for _, p := range prev {
		if p.LastTs != nil {
			priorTs[p.FpHash] = *p.LastTs
		}

		priorDomains[p.FpHash] = p.Domains
	}

	var dsRows []*deviceClassDomainset

	rows := make([]*deviceClassSignal, 0, len(cls))

	for key, a := range cls {
		// gap stats: consecutive gaps over the batch, seeded from the stored cursor
		// (reproduces the former LAG-over-partition exactly — the first-ever row has
		// no prior, matching LAG's NULL).
		sort.Slice(a.tss, func(i, j int) bool { return a.tss[i].Before(a.tss[j]) })

		var gapSum, gapSqSum float64

		var gapN int64

		prevTs, hasPrev := priorTs[key]
		for _, t := range a.tss {
			if hasPrev {
				if g := t.Sub(prevTs).Seconds(); g >= 0 {
					gapSum += g
					gapSqSum += g * g
					gapN++
				}
			}

			prevTs = t
			hasPrev = true
		}

		// capped distinct-eTLD+1 domainset: queue up to (cap − stored) first-seen
		// candidates so the table can never exceed classIoTMaxDomains+1 rows/fp
		// (once >8, the IoT-domain test is decided forever). Duplicates are absorbed
		// by the DoNothing insert; the domains COLUMN is recounted from the table
		// below (never stored+inserted, which would double-count repeat eTLDs).
		allowed := int64(classIoTMaxDomains+1) - priorDomains[key]
		for i, etldp := range a.etldps {
			if int64(i) >= allowed {
				break
			}

			dsRows = append(dsRows, &deviceClassDomainset{FpHash: key, Etldp: etldp})
		}

		// Gap cursor = the batch's TRUE last request ts over ALL rows, decoupled
		// from a.lastTs (which only advances on eTLD-bearing rows). A batch of
		// only empty-eTLD rows leaves a.lastTs at the Go zero time; stored as a
		// non-NULL 0001-01-01 cursor it makes the NEXT batch's first gap ~6.4e10s,
		// dominating gap_sum/gap_sq_sum → CoV → device misclassification. a.tss is
		// sorted ascending above and non-empty whenever this accum exists.
		lastTs := a.lastTs
		if n := len(a.tss); n > 0 {
			lastTs = a.tss[n-1]
		}

		rows = append(rows, &deviceClassSignal{
			FpHash: key, N: a.n,
			ServerHits: a.server, GameHits: a.game, CameraHits: a.camera,
			SpeakerHits: a.speaker, PrinterHits: a.printer, StreamHits: a.stream, PushHits: a.push,
			NASHits: a.nas, RouterHits: a.router, MediaHits: a.media, SmartTVHits: a.smartTV,
			HubHits: a.hub, ThermostatHits: a.thermostat, LightingHits: a.lighting, WearableHits: a.wearable,
			QtypeMask: a.qtypeMask,
			LastTs:    &lastTs, LastDomain: a.lastDomain,
			GapSum: gapSum, GapSqSum: gapSqSum, GapN: gapN,
		})
	}

	// domainset first, so the recount below sees this batch's new eTLDs.
	// DoNothing: membership is idempotent (the cap gate lives in Go).
	if len(dsRows) > 0 {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&dsRows).Error; err != nil {
			return err
		}
	}

	// Absolute domains count from the actual (capped) domainset table.
	type dsCount struct {
		FpHash string
		C      int64
	}

	var counts []dsCount
	if err := tx.Raw(
		"SELECT fp_hash, COUNT(*) AS c FROM device_class_domainset WHERE fp_hash IN ? GROUP BY fp_hash", keys,
	).Scan(&counts).Error; err != nil {
		return err
	}

	domainsOf := map[string]int64{}
	for _, c := range counts {
		domainsOf[c.FpHash] = c.C
	}

	for _, r := range rows {
		r.Domains = domainsOf[r.FpHash]
	}

	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "fp_hash"}},
		DoUpdates: clause.Assignments(map[string]any{
			"n":               gorm.Expr("n + excluded.n"),
			"server_hits":     gorm.Expr("server_hits + excluded.server_hits"),
			"game_hits":       gorm.Expr("game_hits + excluded.game_hits"),
			"camera_hits":     gorm.Expr("camera_hits + excluded.camera_hits"),
			"speaker_hits":    gorm.Expr("speaker_hits + excluded.speaker_hits"),
			"printer_hits":    gorm.Expr("printer_hits + excluded.printer_hits"),
			"stream_hits":     gorm.Expr("stream_hits + excluded.stream_hits"),
			"push_hits":       gorm.Expr("push_hits + excluded.push_hits"),
			"nas_hits":        gorm.Expr("nas_hits + excluded.nas_hits"),
			"router_hits":     gorm.Expr("router_hits + excluded.router_hits"),
			"media_hits":      gorm.Expr("media_hits + excluded.media_hits"),
			"smart_tv_hits":   gorm.Expr("smart_tv_hits + excluded.smart_tv_hits"),
			"hub_hits":        gorm.Expr("hub_hits + excluded.hub_hits"),
			"thermostat_hits": gorm.Expr("thermostat_hits + excluded.thermostat_hits"),
			"lighting_hits":   gorm.Expr("lighting_hits + excluded.lighting_hits"),
			"wearable_hits":   gorm.Expr("wearable_hits + excluded.wearable_hits"),
			"qtype_mask":      gorm.Expr("qtype_mask | excluded.qtype_mask"),
			"gap_sum":         gorm.Expr("gap_sum + excluded.gap_sum"),
			"gap_sq_sum":      gorm.Expr("gap_sq_sum + excluded.gap_sq_sum"),
			"gap_n":           gorm.Expr("gap_n + excluded.gap_n"),
			"domains":         gorm.Expr("excluded.domains"),
			"last_ts":         gorm.Expr("MAX(last_ts, excluded.last_ts)"),
			// last_ts is now the true batch max (decoupled from domains), so a newer
			// empty-eTLD batch must not wipe a stored domain — require a non-empty one.
			"last_domain": gorm.Expr("CASE WHEN excluded.last_ts > last_ts AND excluded.last_domain != '' THEN excluded.last_domain ELSE last_domain END"),
		}),
	}).Create(&rows).Error
}

// pruneServiceUsage size-caps service_usage to the top serviceCapPerDevice by hits
// per fp. Called once per flush from doDBWrite (mirrors pruneNoiseCorpus). Per-
// device and tiny, so it rarely trips.
//
// ponytail: naive window-function DELETE; fine at ~110×32. Add running counters
// only if it shows in a flush-latency profile.
func pruneServiceUsage(tx *gorm.DB) error {
	return tx.Exec(`DELETE FROM service_usage WHERE rowid IN (
		SELECT rowid FROM (
			SELECT rowid, ROW_NUMBER() OVER (PARTITION BY fp_hash ORDER BY hits DESC) AS rn
			FROM service_usage
		) WHERE rn > ?)`, serviceCapPerDevice).Error
}

// categoryFor returns the first catRulesHost category matching the question_name
// (substring), or "" if none. Mirrors ClientCategories' CASE, in Go.
func categoryFor(qn string) string {
	for _, r := range catRulesHost {
		if strings.Contains(qn, r.match) {
			return r.category
		}
	}

	return ""
}

// --- the class scorer (blueprint §4 Scheduler 1) ------------------------------

// classScorerLoop is the LIVE class scorer: every classScorerInterval it reads the
// tiny device_class_signal accumulator and rewrites device_class (and projects
// into client_class for the existing readers) via classify() — NEVER touching
// log_entries, so a class survives a full query-log purge. Modeled on
// blCatsWarmLoop / diskGuardian; lifecycle-tied to Close via classStop/classDone.
func (s *DecoySource) classScorerLoop() {
	defer close(s.classDone)

	ticker := time.NewTicker(classScorerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.classStop:
			return
		case <-ticker.C:
			if err := s.scoreDeviceClasses(); err != nil {
				log.PrefixedLog("heuristics").WithError(err).Warn("class scorer pass failed")
			}

			// Same 5-min tick refreshes the cached client_name→device_key overlay so
			// /clients, /people and /clients/classes keep serving it off the request path.
			s.refreshDominantFP()
		}
	}
}

// scoreDeviceClasses classifies every device from the durable accumulator and
// upserts device_class + client_class, preserving each manual override. classMu
// serializes the ticker against a manual RefreshClientClasses so the two can't
// race the writer connection (non-overlapping guarantee). Ends with a TRUNCATE
// checkpoint to bound the -wal, since this second-writer connection's frames are
// not covered by the flush loop's checkpoint.
func (s *DecoySource) scoreDeviceClasses() error {
	s.classMu.Lock()
	defer s.classMu.Unlock()

	now := time.Now()

	if err := s.evictStaleHeuristics(now.Add(-heuristicsStaleAfter)); err != nil {
		log.PrefixedLog("heuristics").WithError(err).Warn("stale-key eviction failed")
	}

	// Recency bound: idle accumulators can't have changed and their class is
	// already persisted, so only re-score keys active in the window (avoids a
	// full-table load every 5 min). last_ts IS NULL = never-cursored fresh key.
	var sigs []deviceClassSignal
	if err := s.db.Where("last_ts IS NULL OR last_ts >= ?", now.Add(-classScoreRecency)).
		Find(&sigs).Error; err != nil {
		return err
	}

	for i := range sigs {
		class := sigs[i].toFeatures().classify()
		key := sigs[i].FpHash

		if err := s.db.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "fp_hash"}},
			DoUpdates: clause.Assignments(map[string]any{
				"class":      class,
				"updated_at": now,
			}),
		}).Create(&deviceClass{FpHash: key, Class: class, UpdatedAt: now}).Error; err != nil {
			return err
		}

		// Project into client_class (the served table ClientClass reads), preserving
		// the manual override column. This is what makes the served class durable:
		// it is now sourced from the purge-immune accumulator, not a log_entries scan.
		if err := s.db.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "client"}},
			DoUpdates: clause.Assignments(map[string]any{
				"class":      class,
				"updated_at": now,
			}),
		}).Create(&clientClass{Client: key, Class: class, UpdatedAt: now}).Error; err != nil {
			return err
		}
	}

	// Roll up distinct_clients from the durable fp_binding bridge (indexed by
	// idx_fp_binding_fp), replacing the per-snapshot COUNT(DISTINCT fp_hash) scan.
	if err := s.db.Exec(`UPDATE device_identity SET distinct_clients =
		(SELECT COUNT(*) FROM fp_binding WHERE fp_binding.fp_hash = device_identity.fp_hash)`).Error; err != nil {
		return err
	}

	s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")

	return nil
}

// evictStaleHeuristics deletes every heuristics row for device keys silent since
// cutoff (device_identity.last_seen, indexed). This is the growth bound for the
// purge-immune tables: churned IPv6-privacy-address keys stop refreshing
// last_seen and age out; active devices never do. Manual data is preserved:
// persona_link is untouched and device_class rows with a manual override are
// kept. Dependent tables are cleared BEFORE device_identity so the stale-key
// subquery still sees the keys. Cheap when nothing is stale (indexed range scan
// finds an empty set).
func (s *DecoySource) evictStaleHeuristics(cutoff time.Time) error {
	stale := `SELECT fp_hash FROM device_identity WHERE last_seen < ?`

	deps := []string{
		"fp_binding", "device_facet", "device_presence", "service_usage",
		"category_usage", "device_class_signal", "device_class_domainset",
	}
	for _, t := range deps {
		if err := s.db.Exec(
			`DELETE FROM `+t+` WHERE fp_hash IN (`+stale+`)`, cutoff).Error; err != nil {
			return err
		}
	}

	if err := s.db.Exec(
		`DELETE FROM device_class WHERE COALESCE(override,'') = '' AND fp_hash IN (`+stale+`)`,
		cutoff).Error; err != nil {
		return err
	}

	return s.db.Exec(`DELETE FROM device_identity WHERE last_seen < ?`, cutoff).Error
}

// --- small helpers ------------------------------------------------------------

func (id *deviceIdentity) spanSeen(ts time.Time) { spanSeen(&id.FirstSeen, &id.LastSeen, ts) }

func spanSeen(first, last *time.Time, ts time.Time) {
	if ts.Before(*first) {
		*first = ts
	}

	if ts.After(*last) {
		*last = ts
	}
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}

	return b
}

func mapVals[K comparable, V any](m map[K]*V) []*V {
	out := make([]*V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}

	return out
}
