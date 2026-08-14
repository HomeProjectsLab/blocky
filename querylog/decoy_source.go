package querylog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/publicsuffix"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"

	"github.com/0xERR0R/blocky/log"
)

// sessionGap bounds one browsing session for the transition/seed Markov models:
// two consecutive real queries from the same client more than sessionGap apart
// are treated as different sessions, so a transition is only learned across a
// human page-to-page hop, not across an overnight idle. ~30min matches the
// common web-analytics session timeout.
const sessionGap = 30 * time.Minute

// cohortWindow is the ± span around a seed query used to gather a page-load
// cohort (a burst of queries a single page fan-out triggers). A real page load
// resolves its subresources within a second or two; ±this catches the burst
// without merging the next navigation.
const cohortWindow = 2 * time.Second

// revisitMinGap collapses a domain's queries that are closer than this into one
// "visit": a page load fires many queries at the same eTLD+1 within
// milliseconds, and those intra-load repeats are not revisits. Consecutive
// visits are what feed RevisitInterval's cadence median.
const revisitMinGap = 5 * time.Minute

// decoyReplayWindow bounds the replay pool to recent real queries: sampling
// from a rolling window keeps replays plausible (an observer subtracting an
// old, no-longer-visited domain would stand out) and keeps ORDER BY RANDOM()
// cheap (the request_ts index bounds the scan).
const decoyReplayWindow = 7 * 24 * time.Hour

// decoySeedBatch is the insert batch size used when seeding decoy_domains, so a
// 1M-row list never materializes as a single in-RAM slice or one giant INSERT.
const decoySeedBatch = 2000

// noiseCorpusCap bounds the persistent visited-domains corpus (LRU by last_seen).
// ~100k domains is well beyond any household's real footprint yet keeps the table
// small (a few MB) and COUNT/prune cheap. A var (not const) only so the prune test
// can lower it; not config — no knob until someone actually needs to tune it.
var noiseCorpusCap int64 = 100_000

// blockResampleTries bounds the reject-and-resample loop when a sampled decoy
// domain turns out to be blocked: after this many blocked draws we give up and
// return "" rather than ever emitting a domain the box would itself block.
const blockResampleTries = 8

// DecoySource is a read-write handle on the query-log sqlite database used by
// the decoy engine. It co-locates two noise sources on one connection: the
// static Tranco list (decoy_domains, seeded once) sampled by random rowid, and
// the real-query replay pool (recent non-decoy log_entries rows).
type DecoySource struct {
	db *gorm.DB

	mu         sync.Mutex
	rnd        *rand.Rand
	maxRowid   int64 // cached after seeding; decoy_domains is insert-only read-only
	blMaxRowid int64 // cached max rowid of blocklist_domains for random-rowid sampling
}

// decoyDomain is the gorm model for the seeded Tranco list. rowid is SQLite's
// implicit primary key (INTEGER PRIMARY KEY aliases it); inserts are contiguous
// so random-rowid sampling has no gaps.
type decoyDomain struct {
	Rowid  int64  `gorm:"column:rowid;primaryKey"`
	Domain string `gorm:"column:domain;not null"`
}

func (decoyDomain) TableName() string { return "decoy_domains" }

// RealQuery is one sampled row from the replay pool.
type RealQuery struct {
	Name  string `gorm:"column:question_name"`
	Qtype string `gorm:"column:question_type"`
}

// NewDecoySource opens the query-log database read-write (same WAL DSN as the
// writer, so both connections share the file). The writer creates the file and
// the log_entries table before this is called.
func NewDecoySource(sqlitePath string) (*DecoySource, error) {
	dialector, err := newSQLiteDialector(sqlitePath)
	if err != nil {
		return nil, err
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: gormlogger.New(log.Log(), gormlogger.Config{
			SlowThreshold: time.Minute,
			LogLevel:      gormlogger.Warn,
			Colorful:      false,
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("can't open query log database for decoy source: %w", err)
	}

	// Serialize this handle onto a single connection, exactly like the writer
	// (see newDatabaseWriter). This handle both reads (list/corpus/cohort samples,
	// window-scan session models) AND writes (AddToCorpus, blocklist/decoy refresh,
	// list_meta). An unbounded pool lets its own concurrent writers contend on the
	// one WAL file and surface transient "database is locked" under load; one
	// connection makes all of this handle's access queue in-process instead. Its
	// single writer still coexists with the writer's connection via busy_timeout.
	// Decoy sampling is low-frequency (noise cadence), so serializing costs nothing.
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("can't access decoy source connection pool: %w", err)
	}

	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(&decoyDomain{}, &blocklistDomain{}, &listMeta{}, &noiseCorpus{}, &clientClass{}); err != nil {
		return nil, fmt.Errorf("can't create list tables: %w", err)
	}

	s := &DecoySource{db: db, rnd: rand.New(rand.NewSource(time.Now().UnixNano()))} //nolint:gosec // noise timing, not crypto

	if err := s.loadMaxRowid(); err != nil {
		return nil, err
	}

	if err := s.loadBlMaxRowid(); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *DecoySource) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}

	return sqlDB.Close()
}

func (s *DecoySource) loadMaxRowid() error {
	return s.db.Raw("SELECT COALESCE(MAX(rowid),0) FROM decoy_domains").Scan(&s.maxRowid).Error
}

// SeedIfEmpty streams normalized domains (one per line, already decompressed)
// into decoy_domains, but only when the table is empty. Blank lines are
// skipped. Returns the number of rows inserted (0 when already seeded).
func (s *DecoySource) SeedIfEmpty(r io.Reader) (int, error) {
	if s.maxRowid > 0 {
		return 0, nil
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) //nolint:mnd // allow long lines

	batch := make([]decoyDomain, 0, decoySeedBatch)
	inserted := 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}

		if err := s.db.Create(&batch).Error; err != nil {
			return fmt.Errorf("can't seed decoy_domains: %w", err)
		}

		inserted += len(batch)
		batch = batch[:0]

		return nil
	}

	for scanner.Scan() {
		domain := scanner.Text()
		if domain == "" {
			continue
		}

		batch = append(batch, decoyDomain{Domain: domain})
		if len(batch) >= decoySeedBatch {
			if err := flush(); err != nil {
				return inserted, err
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return inserted, fmt.Errorf("can't read decoy list: %w", err)
	}

	if err := flush(); err != nil {
		return inserted, err
	}

	return inserted, s.loadMaxRowid()
}

// SampleList returns a domain from the static Tranco list, biased toward popular
// (head) ranks and never returning a domain the box would itself block.
//
// Distribution: rows are inserted in Tranco rank order, so rowid == rank. We draw
// rowid = floor(max^U), U~Uniform[0,1), giving density P(rowid=r) ∝ 1/r (Zipf,
// s≈1) — a realistic web-popularity shape where head domains recur often but the
// long tail still surfaces, instead of the old uniform draw that made rank 1 and
// rank 1,000,000 equally likely. O(1): one uniform draw, one pow, one indexed
// gap-tolerant row fetch.
//
// Blocked domains (present in blocklist_domains) are rejected and resampled up to
// blockResampleTries times; on exhaustion returns "". Empty string also when the
// list is not seeded.
func (s *DecoySource) SampleList() (string, error) {
	for i := 0; i < blockResampleTries; i++ {
		domain, err := s.sampleListOnce()
		if err != nil || domain == "" {
			return domain, err
		}

		blocked, err := s.IsBlockedDomain(domain)
		if err != nil {
			return "", err
		}

		if !blocked {
			return domain, nil
		}
	}

	return "", nil
}

func (s *DecoySource) sampleListOnce() (string, error) {
	s.mu.Lock()
	max := s.maxRowid
	k := int64(0)
	if max > 0 {
		k = zipfRowid(s.rnd.Float64(), max)
	}
	s.mu.Unlock()

	if max == 0 {
		return "", nil
	}

	var domain string
	err := s.db.Raw("SELECT domain FROM decoy_domains WHERE rowid >= ? ORDER BY rowid LIMIT 1", k).Scan(&domain).Error

	return domain, err
}

// zipfRowid maps a uniform draw u in [0,1) to a rowid in [1,max] with density
// proportional to 1/rowid (Zipf, s≈1): rowid = floor(max^u) has CDF u, so lower
// (more popular) rowids are drawn far more often while the tail stays reachable.
func zipfRowid(u float64, max int64) int64 {
	k := int64(math.Pow(float64(max), u))
	if k < 1 {
		k = 1
	}

	if k > max {
		k = max
	}

	return k
}

// IsBlockedDomain reports whether domain is in the active denylist corpus
// (blocklist_domains, which holds only the enabled categories), via an exact
// indexed lookup on the domain column (idx_blocklist_domain). A domain the box
// would BLOCK must never be one it EMITS as chaff.
//
// ponytail: exact match only. It does not catch a decoy that is a SUBDOMAIN of a
// blocked parent, nor manual denylist_entry rows (those live in the separate
// configstore DB — a cross-DB check isn't worth a second connection here). Upgrade
// to a parent-walk / eTLD+1 check if subdomain leakage ever matters.
func (s *DecoySource) IsBlockedDomain(domain string) (bool, error) {
	if domain == "" {
		return false, nil
	}

	var one int

	err := s.db.Raw("SELECT 1 FROM blocklist_domains WHERE domain = ? LIMIT 1", domain).Scan(&one).Error
	if err != nil {
		return false, err
	}

	return one == 1, nil
}

// HourlyRealCounts returns real (non-decoy) query counts bucketed by UTC
// hour-of-day over the recent window, read from the agg_hourly aggregate table
// (which already excludes decoy rows). Used to shape the decoy rate to real
// activity. A missing agg_hourly table (non-sqlite target, or before the writer
// creates it) surfaces as an error and the caller treats it as cold start.
func (s *DecoySource) HourlyRealCounts() ([24]int64, error) {
	var counts [24]int64

	since := time.Now().Add(-decoyReplayWindow)

	var rows []struct {
		Hour time.Time `gorm:"column:hour"`
		Cnt  int64     `gorm:"column:cnt"`
	}

	if err := s.db.Raw(`SELECT hour, cnt FROM agg_hourly WHERE hour >= ?`, since.UTC()).Scan(&rows).Error; err != nil {
		return counts, err
	}

	for _, r := range rows {
		counts[r.Hour.UTC().Hour()] += r.Cnt
	}

	return counts, nil
}

// FpSample is the complete wire-relevant shape sampled from one real client
// query, enough to rebuild a matching OPT record and question on a decoy.
type FpSample struct {
	Qtype        string   // question type of the sampled real query
	QClass       uint16   // question class (0 == not recorded, treat as IN)
	HadEDNS0     bool     // whether the real query carried an OPT record
	EDNSUDPSize  uint16   // advertised UDP payload size
	DO           bool     // DNSSEC OK bit
	EDNSOptCodes []uint16 // EDNS option codes in wire order — the discriminating signal
	HasCookie    bool     // real query carried an EDNS0 COOKIE
	Mixed0x20    bool     // real qname carried mixed (0x20-randomized) case
}

// fpRow is the set of log_entries columns that reconstruct a wire fingerprint.
type fpRow struct {
	QuestionType string `gorm:"column:question_type"`
	EDNSUDPSize  uint16 `gorm:"column:edns_udp_size"`
	EDNSOptCodes string `gorm:"column:edns_opt_codes"`
	FpDetail     string `gorm:"column:fp_detail"`
}

// toFpSample maps the stored columns + fp_detail JSON blob into an FpSample.
func (r fpRow) toFpSample() FpSample {
	fp := FpSample{
		Qtype:        r.QuestionType,
		EDNSUDPSize:  r.EDNSUDPSize,
		EDNSOptCodes: parseOptCodes(r.EDNSOptCodes),
	}

	// DO/HadEDNS0/qclass/cookie/0x20 live inside the fp_detail JSON blob.
	if r.FpDetail != "" {
		var d struct {
			QClass    uint16 `json:"qclass"`
			DO        bool   `json:"do"`
			HadEDNS0  bool   `json:"hadEdns0"`
			HasCookie bool   `json:"hasCookie"`
			Mixed0x20 bool   `json:"mixed0x20"`
		}

		if json.Unmarshal([]byte(r.FpDetail), &d) == nil {
			fp.QClass = d.QClass
			fp.DO = d.DO
			fp.HadEDNS0 = d.HadEDNS0
			fp.HasCookie = d.HasCookie
			fp.Mixed0x20 = d.Mixed0x20
		}
	}

	return fp
}

// SampleRealFingerprint returns the wire shape of one random recent real query
// so decoys can be stamped to match the real client-software distribution
// (otherwise every synthetic query looks like the resolver's default). Zero
// value (HadEDNS0 false) at cold start — caller then leaves the plain query.
// Use this for list/corpus domains that have no real history of their own; use
// SampleFingerprintForName when replaying a name the box has actually resolved.
func (s *DecoySource) SampleRealFingerprint() (FpSample, error) {
	since := time.Now().Add(-decoyReplayWindow)

	var row fpRow

	err := s.db.Raw(`SELECT question_type, edns_udp_size, edns_opt_codes, fp_detail FROM log_entries
		WHERE decoy = 0 AND question_name <> '' AND request_ts >= ?
		ORDER BY RANDOM() LIMIT 1`, since).Scan(&row).Error
	if err != nil {
		return FpSample{}, err
	}

	return row.toFpSample(), nil
}

// SampleFingerprintForName returns the wire fingerprint most recently observed
// for name's eTLD+1, so a given decoy domain presents a STABLE, plausible OPT
// shape across re-emissions — a domain whose OPT shape flickers between
// appearances is itself a tell. Falls back to SampleRealFingerprint (a random
// recent real fp) when this box has never resolved that eTLD+1, e.g. a Tranco or
// corpus domain that was never a real query here. No time window: a stable shape
// for a domain visited months ago still beats an unrelated random one.
func (s *DecoySource) SampleFingerprintForName(name string) (FpSample, error) {
	etldp := effectiveTLDP(name)
	if etldp == "" {
		return s.SampleRealFingerprint()
	}

	var row fpRow

	err := s.db.Raw(`SELECT question_type, edns_udp_size, edns_opt_codes, fp_detail FROM log_entries
		WHERE decoy = 0 AND effective_tldp = ?
		ORDER BY request_ts DESC LIMIT 1`, etldp).Scan(&row).Error
	if err != nil {
		return FpSample{}, err
	}

	if row.QuestionType == "" { // no real history for this eTLD+1
		return s.SampleRealFingerprint()
	}

	return row.toFpSample(), nil
}

// ClientPersona is a sampled real client's on-wire identity: its source IP and a
// representative EDNS/fingerprint shape it presented. The decoy engine stamps
// these onto decoys (personaAttribution) so chaff is attributed to plausible real
// clients and each client's wire profile stays consistent under noise. A real
// stub's OPT shape is stable across its queries, so one sampled row is enough.
type ClientPersona struct {
	IP string
	Fp FpSample
}

// SampleClient returns a random recent real client (its IP + the fingerprint of
// one of its rows). Empty IP at cold start (no real rows yet). One indexed random
// row read — same cost profile as SampleRealFingerprint.
func (s *DecoySource) SampleClient() (ClientPersona, error) {
	since := time.Now().Add(-decoyReplayWindow)

	var row struct {
		fpRow

		ClientIP string `gorm:"column:client_ip"`
	}

	err := s.db.Raw(`SELECT client_ip, question_type, edns_udp_size, edns_opt_codes, fp_detail FROM log_entries
		WHERE decoy = 0 AND client_ip <> '' AND request_ts >= ?
		ORDER BY RANDOM() LIMIT 1`, since).Scan(&row).Error
	if err != nil {
		return ClientPersona{}, err
	}

	return ClientPersona{IP: row.ClientIP, Fp: row.fpRow.toFpSample()}, nil
}

// effectiveTLDP returns name's registrable domain (eTLD+1), or "" if it has none.
func effectiveTLDP(name string) string {
	e, err := publicsuffix.EffectiveTLDPlusOne(strings.TrimSuffix(name, "."))
	if err != nil {
		return ""
	}

	return e
}

// parseOptCodes turns the stored "10,8,12" wire-order column into codes. Bad
// tokens are skipped so a malformed row degrades to fewer codes, never an error.
func parseOptCodes(s string) []uint16 {
	if s == "" {
		return nil
	}

	parts := strings.Split(s, ",")
	codes := make([]uint16, 0, len(parts))

	for _, p := range parts {
		if n, err := strconv.ParseUint(strings.TrimSpace(p), 10, 16); err == nil {
			codes = append(codes, uint16(n))
		}
	}

	return codes
}

// SampleRecentReal returns up to limit real (non-decoy) queries sampled at
// random from the recent window, excluding any name the box would itself block
// (set-based NOT EXISTS against blocklist_domains, so no resample loop is needed).
// Empty slice at cold start (no history yet).
func (s *DecoySource) SampleRecentReal(limit int) ([]RealQuery, error) {
	since := time.Now().Add(-decoyReplayWindow)

	var out []RealQuery
	err := s.db.Raw(`SELECT question_name, question_type FROM log_entries
		WHERE decoy = 0 AND question_name <> '' AND request_ts >= ?
		AND NOT EXISTS (SELECT 1 FROM blocklist_domains b WHERE b.domain = log_entries.question_name)
		ORDER BY RANDOM() LIMIT ?`, since, limit).Scan(&out).Error

	return out, err
}

// --- persistent visited-domains noise corpus (T3) ---------------------------

// noiseCorpus is the PER-INSTANCE, NEVER-EXPORTED durable record of every domain
// this box's own clients have ever resolved (real, non-blocked queries). Unlike
// the 7-day log window that SampleRecentReal reads, a domain visited once stays
// available as chaff forever, so a personal/rare domain (bank, *.lan, internal
// SSO) can't be unmasked by an observer who simply notes it stopped appearing.
// LRU-capped at noiseCorpusCap by last_seen.
//
// PRIVACY: this table is a fingerprint of the household's browsing history. It is
// per-instance state and MUST NEVER be exported, synced, or shared off the box.
type noiseCorpus struct {
	Domain    string    `gorm:"column:domain;primaryKey"`
	FirstSeen time.Time `gorm:"column:first_seen"`
	LastSeen  time.Time `gorm:"column:last_seen;index"`
	Hits      int64     `gorm:"column:hits"`
}

func (noiseCorpus) TableName() string { return "noise_corpus" }

// upsertNoiseCorpus folds a raw batch into noise_corpus inside tx: every real
// (non-decoy), non-blocked, non-empty question_name bumps hits and last_seen and
// records first_seen once. Blocked rows (ResponseType == "BLOCKED") are skipped so
// the box never learns to emit what it blocks. sqlite-only; called from
// DatabaseWriter.doDBWrite in the same transaction as the raw insert + aggregates.
func upsertNoiseCorpus(tx *gorm.DB, entries []*logEntry) error {
	agg := map[string]*noiseCorpus{}

	for _, e := range entries {
		if e.Decoy || e.ResponseType == "BLOCKED" || e.QuestionName == "" {
			continue
		}

		c := agg[e.QuestionName]
		if c == nil {
			c = &noiseCorpus{Domain: e.QuestionName, FirstSeen: e.RequestTS, LastSeen: e.RequestTS}
			agg[e.QuestionName] = c
		}

		c.Hits++

		if e.RequestTS.Before(c.FirstSeen) {
			c.FirstSeen = e.RequestTS
		}

		if e.RequestTS.After(c.LastSeen) {
			c.LastSeen = e.RequestTS
		}
	}

	if len(agg) == 0 {
		return nil
	}

	rows := make([]*noiseCorpus, 0, len(agg))
	for _, c := range agg {
		rows = append(rows, c)
	}

	// first_seen is deliberately not in DoUpdates: keep the earliest observation.
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "domain"}},
		DoUpdates: clause.Assignments(map[string]any{
			"hits":      gorm.Expr("hits + excluded.hits"),
			"last_seen": gorm.Expr("MAX(last_seen, excluded.last_seen)"),
		}),
	}).Create(&rows).Error
}

// pruneNoiseCorpus enforces the LRU cap: when the corpus exceeds noiseCorpusCap it
// deletes the oldest-by-last_seen rows down to the cap. Called once per flush from
// doDBWrite (not per batch).
//
// ponytail: naive COUNT-per-flush; at the 100k cap it is a few ms on the write
// connection. Maintain a running counter if flush latency ever shows up.
func pruneNoiseCorpus(tx *gorm.DB) error {
	var n int64
	if err := tx.Raw("SELECT COUNT(*) FROM noise_corpus").Scan(&n).Error; err != nil {
		return err
	}

	if n <= noiseCorpusCap {
		return nil
	}

	return tx.Exec(`DELETE FROM noise_corpus WHERE domain IN (
		SELECT domain FROM noise_corpus ORDER BY last_seen ASC LIMIT ?)`, n-noiseCorpusCap).Error
}

// SampleCorpus samples a domain from the persistent visited-domains corpus,
// mildly biased toward heavy hitters (best-of-two by hits: two uniform
// random-rowid draws, higher-hits wins) so popular personal domains recur more
// often without dominating. Rejects blocked domains (resample up to
// blockResampleTries). Empty string when the corpus is empty. Cheap: O(1) indexed
// lookups per draw.
//
// PER-INSTANCE ONLY — this samples the box's own browsing history; never shared.
func (s *DecoySource) SampleCorpus() (string, error) {
	for i := 0; i < blockResampleTries; i++ {
		domain, err := s.sampleCorpusOnce()
		if err != nil || domain == "" {
			return domain, err
		}

		blocked, err := s.IsBlockedDomain(domain)
		if err != nil {
			return "", err
		}

		if !blocked {
			return domain, nil
		}
	}

	return "", nil
}

func (s *DecoySource) sampleCorpusOnce() (string, error) {
	var max int64
	if err := s.db.Raw("SELECT COALESCE(MAX(rowid),0) FROM noise_corpus").Scan(&max).Error; err != nil {
		return "", err
	}

	if max == 0 {
		return "", nil
	}

	aDomain, aHits, err := s.corpusRowAt(max)
	if err != nil {
		return "", err
	}

	bDomain, bHits, err := s.corpusRowAt(max)
	if err != nil {
		return "", err
	}

	if bHits > aHits {
		return bDomain, nil
	}

	return aDomain, nil
}

// corpusRowAt draws one random rowid in [1,max] and returns the first row at or
// after it (gap-tolerant across LRU-pruned holes).
func (s *DecoySource) corpusRowAt(max int64) (string, int64, error) {
	s.mu.Lock()
	k := s.rnd.Int63n(max) + 1
	s.mu.Unlock()

	var row struct {
		Domain string `gorm:"column:domain"`
		Hits   int64  `gorm:"column:hits"`
	}

	err := s.db.Raw("SELECT domain, hits FROM noise_corpus WHERE rowid >= ? ORDER BY rowid LIMIT 1", k).Scan(&row).Error

	return row.Domain, row.Hits, err
}

// AddToCorpus inserts or refreshes domain in the persistent visited-domains
// corpus (noise_corpus), the same table the real-query write path populates. It
// bumps hits and moves last_seen forward (first_seen kept), so a pre-warmed
// domain is sampled by SampleCorpus like any real visit. PER-INSTANCE ONLY —
// never exported. No-op on an empty domain.
func (s *DecoySource) AddToCorpus(domain string) error {
	if domain == "" {
		return nil
	}

	now := time.Now()

	return s.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "domain"}},
		DoUpdates: clause.Assignments(map[string]any{
			"hits":      gorm.Expr("hits + 1"),
			"last_seen": gorm.Expr("MAX(last_seen, excluded.last_seen)"),
		}),
	}).Create(&noiseCorpus{Domain: domain, FirstSeen: now, LastSeen: now, Hits: 1}).Error
}

// --- cohort / session / revisit models (derived read-side from log_entries) --

// CohortMember is one query of a real page-load cohort, in recorded time order.
// The first member (DelayMs == 0) is the primary navigation; the rest are the
// subresource fan-out at their observed offsets. Blocked members (trackers the
// box denied) are included so the cohort mirrors the real page — the decoy
// engine decides which to actually emit.
type CohortMember struct {
	Domain  string // question_name as resolved
	Qtype   uint16 // DNS type (A/AAAA/HTTPS/…), converted from the stored string
	DelayMs int    // offset from the cohort's first query, milliseconds
	Blocked bool   // response_type == "BLOCKED"
}

// cohortRow is the log_entries projection SampleCohort reads.
type cohortRow struct {
	QuestionName string    `gorm:"column:question_name"`
	QuestionType string    `gorm:"column:question_type"`
	ResponseType string    `gorm:"column:response_type"`
	RequestTS    time.Time `gorm:"column:request_ts"`
}

// SampleCohort picks a random real page-load burst and returns its members in
// recorded time order, each carrying its delay from the burst's first query.
//
// Derivation: seed on a random recent real (non-decoy) row, then gather that
// same client's real rows within ±cohortWindow of the seed and order by
// request_ts. Blocked rows are NOT filtered — a real page's tracker/ad lookups
// are part of its fingerprint, so the cohort includes them (Blocked=true) and
// the engine chooses emission. The earliest row is the primary. Empty cohort
// (cold start, no real rows yet) → nil, nil.
//
// O(window): the seed is one indexed random row; the gather rides the
// (client_name, request_ts) composite index.
//
// ponytail: on-demand burst grouping, one seed + one ranged read per call. A
// symmetric ±window can occasionally merge two back-to-back navigations into
// one cohort; harmless for noise. Materialize a cohort table if this ever gets
// heavy or the merge matters.
func (s *DecoySource) SampleCohort() ([]CohortMember, error) {
	since := time.Now().Add(-decoyReplayWindow)

	var seed struct {
		ClientName string    `gorm:"column:client_name"`
		RequestTS  time.Time `gorm:"column:request_ts"`
		Found      bool      `gorm:"column:found"`
	}

	err := s.db.Raw(`SELECT client_name, request_ts, 1 AS found FROM log_entries
		WHERE decoy = 0 AND question_name <> '' AND request_ts >= ?
		ORDER BY RANDOM() LIMIT 1`, since).Scan(&seed).Error
	if err != nil {
		return nil, err
	}

	if !seed.Found {
		return nil, nil // cold start
	}

	var rows []cohortRow

	err = s.db.Raw(`SELECT question_name, question_type, response_type, request_ts FROM log_entries
		WHERE decoy = 0 AND question_name <> '' AND client_name = ?
		AND request_ts BETWEEN ? AND ?
		ORDER BY request_ts ASC`,
		seed.ClientName, seed.RequestTS.Add(-cohortWindow), seed.RequestTS.Add(cohortWindow)).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	if len(rows) == 0 {
		return nil, nil
	}

	first := rows[0].RequestTS
	out := make([]CohortMember, 0, len(rows))

	for _, r := range rows {
		out = append(out, CohortMember{
			Domain:  r.QuestionName,
			Qtype:   qtypeFromString(r.QuestionType),
			DelayMs: int(r.RequestTS.Sub(first).Milliseconds()),
			Blocked: r.ResponseType == "BLOCKED",
		})
	}

	return out, nil
}

// NextInSession returns a plausible next primary (eTLD+1) to follow
// primaryDomain, drawn from real same-session transitions observed on this box.
//
// Derivation: a Markov step over the real timeline. Per client, consecutive
// real rows (ordered by time, within sessionGap) whose eTLD+1 changes form a
// transition cur→next; counts over the recent window are the weights, and one
// weighted draw picks the successor. Returns "" when primaryDomain has no known
// successor (cold start or a leaf domain) so the engine falls back to a fresh
// source pick.
//
// ponytail: one windowed full scan of log_entries per call (LEAD over the
// timeline; effective_tldp is unindexed). Fine at noise cadence over a 7-day
// window; materialize a transitions table if it shows up hot.
func (s *DecoySource) NextInSession(primaryDomain string) (string, error) {
	if primaryDomain == "" {
		return "", nil
	}

	since := time.Now().Add(-decoyReplayWindow)
	gapSecs := sessionGap.Seconds()

	var rows []struct {
		Domain string `gorm:"column:d"`
		Cnt    int64  `gorm:"column:c"`
	}

	err := s.db.Raw(`SELECT nxt AS d, COUNT(*) AS c FROM (
		SELECT effective_tldp AS cur,
		       LEAD(effective_tldp) OVER w AS nxt,
		       (julianday(LEAD(request_ts) OVER w) - julianday(request_ts))*86400 AS gap
		FROM log_entries
		WHERE decoy = 0 AND effective_tldp <> '' AND request_ts >= ?
		WINDOW w AS (PARTITION BY client_name ORDER BY request_ts)
	)
	WHERE cur = ? AND nxt IS NOT NULL AND nxt <> cur AND gap >= 0 AND gap <= ?
	GROUP BY nxt`, since, primaryDomain, gapSecs).Scan(&rows).Error
	if err != nil {
		return "", err
	}

	var total int64
	for _, r := range rows {
		total += r.Cnt
	}

	if total == 0 {
		return "", nil
	}

	s.mu.Lock()
	pick := s.rnd.Int63n(total)
	s.mu.Unlock()

	for _, r := range rows {
		pick -= r.Cnt
		if pick < 0 {
			return r.Domain, nil
		}
	}

	return rows[len(rows)-1].Domain, nil
}

// SessionSeed returns a plausible session-STARTING primary (eTLD+1): a domain
// that historically begins sessions (its client's first real query, or the
// first after a > sessionGap idle), weighted by how often it does so. "" at
// cold start. Same windowed-scan cost profile as NextInSession.
func (s *DecoySource) SessionSeed() (string, error) {
	since := time.Now().Add(-decoyReplayWindow)
	gapSecs := sessionGap.Seconds()

	var rows []struct {
		Domain string `gorm:"column:d"`
		Cnt    int64  `gorm:"column:c"`
	}

	err := s.db.Raw(`SELECT cur AS d, COUNT(*) AS c FROM (
		SELECT effective_tldp AS cur,
		       (julianday(request_ts) - julianday(LAG(request_ts) OVER w))*86400 AS gap
		FROM log_entries
		WHERE decoy = 0 AND effective_tldp <> '' AND request_ts >= ?
		WINDOW w AS (PARTITION BY client_name ORDER BY request_ts)
	)
	WHERE gap IS NULL OR gap > ?
	GROUP BY cur`, since, gapSecs).Scan(&rows).Error
	if err != nil {
		return "", err
	}

	var total int64
	for _, r := range rows {
		total += r.Cnt
	}

	if total == 0 {
		return "", nil
	}

	s.mu.Lock()
	pick := s.rnd.Int63n(total)
	s.mu.Unlock()

	for _, r := range rows {
		pick -= r.Cnt
		if pick < 0 {
			return r.Domain, nil
		}
	}

	return rows[len(rows)-1].Domain, nil
}

// RevisitInterval returns the typical gap between real visits to domain's
// eTLD+1 (the median of consecutive-visit deltas) and ok=true, or ok=false when
// there are fewer than two distinct visits to derive an interval from. The
// engine uses it to re-emit a corpus domain on a human-plausible cadence
// instead of flat Poisson.
//
// A "visit" collapses queries closer than revisitMinGap (one page load fires
// many queries at the same eTLD+1 within milliseconds; those are not revisits).
// Scans the recent window only, so a daily domain yields several samples while
// the scan stays bounded.
//
// ponytail: median over an unindexed effective_tldp scan; bounded by the
// window and one specific domain, so cheap in practice.
func (s *DecoySource) RevisitInterval(domain string) (time.Duration, bool) {
	etldp := effectiveTLDP(domain)
	if etldp == "" {
		return 0, false
	}

	since := time.Now().Add(-decoyReplayWindow)

	var ts []time.Time

	err := s.db.Raw(`SELECT request_ts FROM log_entries
		WHERE decoy = 0 AND effective_tldp = ? AND request_ts >= ?
		ORDER BY request_ts ASC`, etldp, since).Scan(&ts).Error
	if err != nil || len(ts) < 2 {
		return 0, false
	}

	// Collapse intra-page-load bursts into single visits, then delta them.
	deltas := make([]time.Duration, 0, len(ts))
	last := ts[0]

	for _, t := range ts[1:] {
		d := t.Sub(last)
		if d < revisitMinGap {
			continue // same visit
		}

		deltas = append(deltas, d)
		last = t
	}

	if len(deltas) == 0 {
		return 0, false // one visit after collapsing
	}

	sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })

	return deltas[len(deltas)/2], true // lower-median; noise cadence, not statistics
}

// --- device-class classification (cached) -----------------------------------

// Device classes a client can be assigned. "unknown" covers too-little-data.
const (
	ClassIoT         = "iot"
	ClassWorkstation = "workstation"
	ClassServer      = "server"
	ClassUnknown     = "unknown"
)

// validClass reports whether c is an assignable device class (empty = clear override).
func validClass(c string) bool {
	switch c {
	case ClassIoT, ClassWorkstation, ClassServer, ClassUnknown:
		return true
	default:
		return false
	}
}

// Classification thresholds. Heuristic; tuned for a mixed edge/office/IoT fleet.
// A client with fewer than classMinSamples real queries in the window is "unknown"
// (not enough behaviour to judge). "server" wins on a high share of registry/
// update/monitoring hits. "iot" is few distinct eTLD+1s + low qtype diversity +
// regular (low-burstiness) inter-arrivals — a beacon, not a browser. Everything
// else with real breadth is "workstation".
const (
	classMinSamples    = 20   // below this many real queries → unknown
	classServerShare   = 0.30 // ≥ this share of server-ish hits → server
	classIoTMaxDomains = 8    // iot: at most this many distinct eTLD+1
	classIoTMaxQtypes  = 3    // iot: at most this many distinct qtypes
	classIoTMaxCoV     = 2.0  // iot: inter-arrival coeff. of variation ≤ this (regular/periodic)
)

// serverEtldps is the small embedded "server-ish" registrable-domain set: container
// registries, OS package mirrors, and update/monitoring backends. A client whose
// traffic is dominated by these is a server, not a workstation. Kept deliberately
// tiny — a heuristic seed, not an exhaustive list.
var serverEtldps = []string{
	"docker.io", "ghcr.io", "quay.io", "gcr.io", "k8s.io", "docker.com",
	"debian.org", "ubuntu.com", "archlinux.org", "fedoraproject.org", "centos.org",
	"pypi.org", "npmjs.org", "npmjs.com", "rubygems.org", "crates.io", "golang.org",
	"githubusercontent.com", "github.com", "grafana.com", "prometheus.io",
	"cloudflare.com", "letsencrypt.org", "ntp.org",
}

// serverNameLikes are substring patterns in question_name that flag server-ish
// package/update traffic regardless of eTLD+1. Deliberately NOT "telemetry" or
// "metrics" — those are exactly what IoT devices beacon, so matching them would
// misclassify IoT as server. Server = package registries / OS mirrors / updates.
var serverNameLikes = []string{"registry", "apt.", "mirror", "-update", "update."}

// clientClass is the cached per-client classification. `class` is the auto result;
// `override` (when non-empty) is a UI-set manual class that wins over auto.
// Recomputed by RefreshClientClasses; read cheaply by ClientClass.
type clientClass struct {
	Client    string    `gorm:"column:client;primaryKey"`
	Class     string    `gorm:"column:class"`
	Override  string    `gorm:"column:override"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (clientClass) TableName() string { return "client_class" }

// ClientClassInfo is one row of ListClientClasses: the auto class, any override,
// and the Effective class the engine actually uses (override if set, else auto).
type ClientClassInfo struct {
	Client    string
	Class     string // auto-computed
	Override  string // manual, "" if none
	Effective string // override if non-empty, else Class
	UpdatedAt time.Time
}

// classFeatures is the per-client aggregate the classifier scores.
type classFeatures struct {
	Client     string  `gorm:"column:c"`
	N          int64   `gorm:"column:n"`
	Domains    int64   `gorm:"column:domains"`
	Qtypes     int64   `gorm:"column:qtypes"`
	ServerHits int64   `gorm:"column:serverhits"`
	MeanGap    float64 `gorm:"column:meangap"`
	MeanGap2   float64 `gorm:"column:meangap2"`
}

// classify scores one client's features into a device class.
func (f classFeatures) classify() string {
	if f.N < classMinSamples {
		return ClassUnknown
	}

	if float64(f.ServerHits)/float64(f.N) >= classServerShare {
		return ClassServer
	}

	// Inter-arrival coefficient of variation: low = regular/periodic (beacon).
	// Var = E[g²] − E[g]²; CoV = sqrt(Var)/mean. Guard tiny/zero mean.
	cov := math.MaxFloat64
	if f.MeanGap > 0 {
		v := f.MeanGap2 - f.MeanGap*f.MeanGap
		if v < 0 {
			v = 0
		}

		cov = math.Sqrt(v) / f.MeanGap
	}

	if f.Domains <= classIoTMaxDomains && f.Qtypes <= classIoTMaxQtypes && cov <= classIoTMaxCoV {
		return ClassIoT
	}

	return ClassWorkstation
}

// serverHitExpr builds the SQL boolean (1/0-summable) that flags a server-ish row,
// matching effective_tldp against serverEtldps or question_name against
// serverNameLikes. Values are inlined (fixed, trusted constants — no user input).
func serverHitExpr() string {
	quoted := make([]string, len(serverEtldps))
	for i, d := range serverEtldps {
		quoted[i] = "'" + d + "'"
	}

	likes := make([]string, len(serverNameLikes))
	for i, p := range serverNameLikes {
		likes[i] = "qn LIKE '%" + p + "%'"
	}

	return "CASE WHEN etldp IN (" + strings.Join(quoted, ",") + ") OR " +
		strings.Join(likes, " OR ") + " THEN 1 ELSE 0 END"
}

// RefreshClientClasses recomputes every client's auto class from the recent real
// timeline in one windowed pass and upserts it into client_class. The manual
// override column is preserved (never touched here). Call on a timer / first use —
// NOT per emission (single connection, ~110 clients).
//
// ponytail: one full window scan of log_entries per refresh (LAG over the
// timeline; effective_tldp/question_name unindexed). Fine at refresh cadence over
// the 7-day window; materialize incrementally only if refresh latency shows up.
func (s *DecoySource) RefreshClientClasses() error {
	since := time.Now().Add(-decoyReplayWindow)

	query := `WITH gaps AS (
		SELECT client_name AS c,
		       effective_tldp AS etldp,
		       question_type AS qt,
		       question_name AS qn,
		       (julianday(request_ts) - julianday(LAG(request_ts) OVER w))*86400 AS gap
		FROM log_entries
		WHERE decoy = 0 AND client_name <> '' AND request_ts >= ?
		WINDOW w AS (PARTITION BY client_name ORDER BY request_ts)
	)
	SELECT c,
	       COUNT(*) AS n,
	       COUNT(DISTINCT etldp) AS domains,
	       COUNT(DISTINCT qt) AS qtypes,
	       SUM(` + serverHitExpr() + `) AS serverhits,
	       COALESCE(AVG(gap),0) AS meangap,
	       COALESCE(AVG(gap*gap),0) AS meangap2
	FROM gaps
	GROUP BY c`

	var feats []classFeatures
	if err := s.db.Raw(query, since).Scan(&feats).Error; err != nil {
		return err
	}

	now := time.Now()

	for _, f := range feats {
		row := clientClass{Client: f.Client, Class: f.classify(), UpdatedAt: now}

		// Upsert the auto class + timestamp only; leave override untouched.
		err := s.db.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "client"}},
			DoUpdates: clause.Assignments(map[string]any{
				"class":      row.Class,
				"updated_at": row.UpdatedAt,
			}),
		}).Create(&row).Error
		if err != nil {
			return err
		}
	}

	return nil
}

// ClientClass returns the cached effective class for client (override if set,
// else the auto class), or ClassUnknown when the client has no cached row yet.
// Fast: one primary-key lookup, no scan — safe to call per emission.
func (s *DecoySource) ClientClass(client string) (string, error) {
	var row clientClass

	err := s.db.Raw("SELECT class, override FROM client_class WHERE client = ? LIMIT 1", client).Scan(&row).Error
	if err != nil {
		return ClassUnknown, err
	}

	if row.Override != "" {
		return row.Override, nil
	}

	if row.Class == "" {
		return ClassUnknown, nil
	}

	return row.Class, nil
}

// ListClientClasses returns every cached client classification (auto + override +
// resolved effective), for the management UI.
func (s *DecoySource) ListClientClasses() ([]ClientClassInfo, error) {
	var rows []clientClass
	if err := s.db.Raw("SELECT client, class, override, updated_at FROM client_class ORDER BY client").Scan(&rows).Error; err != nil {
		return nil, err
	}

	out := make([]ClientClassInfo, 0, len(rows))

	for _, r := range rows {
		eff := r.Class
		if r.Override != "" {
			eff = r.Override
		}

		if eff == "" {
			eff = ClassUnknown
		}

		out = append(out, ClientClassInfo{
			Client: r.Client, Class: r.Class, Override: r.Override, Effective: eff, UpdatedAt: r.UpdatedAt,
		})
	}

	return out, nil
}

// SetClientClassOverride sets (or clears) the manual class override for client.
// class "" or "auto" clears the override (auto classification resumes); any other
// value must be a valid device class. The override wins over auto in ClientClass.
// Creates the row if the client hasn't been auto-classified yet, so an override
// set before first refresh still sticks.
func (s *DecoySource) SetClientClassOverride(client, class string) error {
	if client == "" {
		return fmt.Errorf("client must not be empty")
	}

	if class == "auto" {
		class = ""
	}

	if class != "" && !validClass(class) {
		return fmt.Errorf("invalid device class %q", class)
	}

	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "client"}},
		DoUpdates: clause.Assignments(map[string]any{"override": class}),
	}).Create(&clientClass{Client: client, Override: class, UpdatedAt: time.Now()}).Error
}

// SampleClientOfClass returns a random real client whose EFFECTIVE class matches
// class, as a ClientPersona (IP + fingerprint) for decoy attribution. Empty
// ClientPersona (empty IP) when no such client exists — caller falls back to
// SampleClient. Two cheap indexed random-row reads.
func (s *DecoySource) SampleClientOfClass(class string) (ClientPersona, error) {
	var name string

	err := s.db.Raw(`SELECT client FROM client_class
		WHERE CASE WHEN override <> '' THEN override ELSE class END = ?
		ORDER BY RANDOM() LIMIT 1`, class).Scan(&name).Error
	if err != nil {
		return ClientPersona{}, err
	}

	if name == "" {
		return ClientPersona{}, nil
	}

	since := time.Now().Add(-decoyReplayWindow)

	var row struct {
		fpRow

		ClientIP string `gorm:"column:client_ip"`
	}

	err = s.db.Raw(`SELECT client_ip, question_type, edns_udp_size, edns_opt_codes, fp_detail FROM log_entries
		WHERE decoy = 0 AND client_name = ? AND client_ip <> '' AND request_ts >= ?
		ORDER BY RANDOM() LIMIT 1`, name, since).Scan(&row).Error
	if err != nil {
		return ClientPersona{}, err
	}

	return ClientPersona{IP: row.ClientIP, Fp: row.fpRow.toFpSample()}, nil
}

// qtypeFromString maps a stored question_type string ("A", "AAAA", "HTTPS", …)
// to its DNS type code, defaulting to A on an unknown/empty token. (querylog
// can't import decoy's identical helper — that would be an import cycle.)
func qtypeFromString(s string) uint16 {
	if t, ok := dns.StringToType[s]; ok {
		return t
	}

	return dns.TypeA
}
