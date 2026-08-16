package querylog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

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

// cohortMaxMembers caps how many members a sampled cohort may carry: emitCohort
// replays every member upstream, so an uncapped recorded storm would replay as a
// recurring decoy burst (Pi3 CPU + upstream traffic). A real page load is
// typically well under this.
const cohortMaxMembers = 48

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

// maxDecoyReaders sizes the read-only sampling pool. It is the decoy engine's
// maxConcurrentEmits (decoy/engine.go, 8) PLUS uiReadHeadroom so UI-facing reads
// (ListClientClasses/ClientNames/… on the same pool) can never evict an emit
// worker's connection — without the headroom the 8 emit conns and every UI read
// contend the same 8 slots, and a slow UI read starves the emit path (the ~2h
// stall). WAL gives one-writer/many-readers, so these coexist with the pinned-1
// writer. (querylog can't import decoy for the const — decoy imports querylog.)
const uiReadHeadroom = 4
const maxDecoyReaders = 8 + uiReadHeadroom

// uiReadTimeout bounds every UI-facing read on the ro pool via a context deadline,
// so an exhausted/slow pool surfaces as a fast error instead of an unbounded hang
// (busy_timeout only bounds in-DB lock waits, not Go pool-acquisition, which uses
// context.Background() by default — no deadline). Emit samplers keep no UI deadline.
const uiReadTimeout = 5 * time.Second

// fpRefreshTimeout is the budget for the OFF-request overlay recompute
// (dominantFPByName): the windowed GROUP BY can exceed uiReadTimeout on a large
// log, and every caller is a background refresher (boot warm, 5-min tick, kick,
// post-write) — never a UI request — so it gets a generous budget instead of the
// request fail-fast one.
const fpRefreshTimeout = 2 * time.Minute

// fpKickBackoff is how long cold-read kicks stay suppressed after a failed
// overlay recompute, so failure doesn't self-kick a doomed scan per request.
const fpKickBackoff = time.Minute

// leRowidTTL bounds how long the cached MIN/MAX log_entries rowid bounds are
// reused before a refresh. Short, because the disk guardian prunes the oldest
// rows (moving the floor up) on a few-minute cadence — a longer TTL would make
// below-floor draws (wasted resamples) common.
const leRowidTTL = 30 * time.Second

// rowidSeekTries bounds how many random-rowid draws a sampler makes before it
// falls back to a forward scan from the floor. A draw can land in an all-filtered
// tail (only decoy=1 rows, or below the request_ts window) and match nothing; a
// few fresh draws almost always hit, and the floor fallback guarantees an
// existing row is never missed.
const rowidSeekTries = 4

// DecoySource is a read-write handle on the query-log sqlite database used by
// the decoy engine. It co-locates two noise sources: the static Tranco list
// (decoy_domains, seeded once) sampled by random rowid, and the real-query replay
// pool (recent non-decoy log_entries rows).
//
// It holds TWO connection pools: db is pinned to a single connection and is the
// sole WRITER (seeding, corpus upserts, class/profile/identity writes, blocklist
// refresh); ro is a bounded mode=ro READER pool (maxDecoyReaders conns) that
// carries every Sample*/read on the emit hot path, so sampling is not serialized
// behind the one writer connection.
type DecoySource struct {
	db *gorm.DB // pinned-1 writer
	ro *gorm.DB // read-only sampling pool (mode=ro)

	mu         sync.Mutex
	rnd        *rand.Rand
	maxRowid   int64 // cached after seeding; decoy_domains is insert-only read-only
	blMaxRowid int64 // cached max rowid of blocklist_domains for random-rowid sampling

	// cached per-category blocklist counts (the GET /api/ui/blocking read). A full
	// GROUP BY COUNT over blocklist_domains (~3.5M rows) is seconds on a Pi3; counts
	// change only on ReplaceBlocklist/PruneBlocklist, so the result is cached here
	// (guarded by mu), warmed lazily, and invalidated on those write paths.
	blCats      []BlocklistStat
	blCatsValid bool

	// cached client_name→device_key overlay (dominantFPByName). /clients, /people
	// and /clients/classes translate names↔keys through it; recomputing it is a full
	// windowed log_entries GROUP BY that grows to seconds after hours of uptime, so
	// it is served from here and recomputed OFF the request path only (startup warm,
	// the 5-min class tick, and a background refresh after a name/person write). A
	// refresh builds a fresh map and swaps the pointer — readers hold the old map
	// safely, so this is race-clean vs the emit workers. Guarded by mu.
	fpByName      map[string]string
	fpByNameValid bool

	// blWarm signals the long-lived background warmer to recompute blCats after an
	// invalidation, so the ~9s GROUP BY COUNT runs off the request path instead of
	// on the next visitor. Buffered depth 1 => a burst of ~12 ReplaceBlocklist
	// invalidations coalesces into a single pending signal. blWarmStop/blWarmDone
	// tie the goroutine to Close (started in NewDecoySource, mirrors the emit workers).
	blWarm     chan struct{}
	blWarmStop chan struct{}
	blWarmDone chan struct{}

	// class scorer (heuristics.go): the durable-intelligence ticker. classStop/
	// classDone tie the goroutine to Close (mirrors blWarmStop/blWarmDone); classMu
	// serializes the ticker against a manual RefreshClientClasses on the writer conn.
	// classStopOnce makes the stop idempotent (tests halt the scorer early to
	// single-thread s.db, then Close runs the same stop again).
	classStop     chan struct{}
	classDone     chan struct{}
	classStopOnce sync.Once
	classMu       sync.Mutex

	// cached MIN/MAX rowid of log_entries for indexed random-rowid sampling of the
	// real-query replay pool, refreshed on leRowidTTL (all guarded by mu).
	leMinRowid int64
	leMaxRowid int64
	leRowidAt  time.Time

	// lastSampleWarn is the unix-nano of the last emitted sampler-error WARN,
	// used by warnSampleErr to rate-limit (atomic; no lock-order coupling with mu).
	// atomic.Int64 self-aligns on 32-bit ARM/386 (a plain int64 field here is
	// misaligned and atomic ops on it panic).
	lastSampleWarn atomic.Int64

	// fpRefreshing single-flights the off-path overlay recompute: a burst of cold
	// reads (the tiny boot window before the startup warm lands, or after a transient
	// scan error) kicks exactly ONE background refreshDominantFP, never a goroutine
	// per request that would pile the windowed GROUP BY onto the RO pool. Atomic; no
	// lock-order coupling with mu.
	fpRefreshing atomic.Bool

	// fpRetryAt (unix-nano) backs off cold-read kicks after a failed overlay scan:
	// without it every cold read re-kicks a doomed recompute in a tight loop.
	fpRetryAt atomic.Int64
}

// sampleWarnEvery rate-limits the sampler-error WARN: a persistent DB fault
// (locked/corrupt/IO) surfaces on every emit, so an unthrottled WARN here would
// be the firehose this logging pass exists to avoid.
const sampleWarnEvery = 30 * time.Second

// warnSampleErr logs a decoy-source sampler DB error at WARN, rate-limited to one
// line per sampleWarnEvery across all samplers. err==nil and empty/no-row results
// are never errors and never reach here. Returns err unchanged so callers keep
// their existing control flow (additive: log only).
func (s *DecoySource) warnSampleErr(op string, err error) error {
	if err == nil {
		return nil
	}

	now := time.Now().UnixNano()

	last := s.lastSampleWarn.Load()
	if now-last >= int64(sampleWarnEvery) && s.lastSampleWarn.CompareAndSwap(last, now) {
		log.PrefixedLog("decoy_source").WithField("op", op).WithError(err).
			Warn("decoy sampler DB error (rate-limited)")
	}

	return err
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

	migrate := []any{&decoyDomain{}, &blocklistDomain{}, &listMeta{}, &noiseCorpus{}, &clientClass{},
		&clientIdentity{}, &clientPerson{}, &clientProfile{}, &decoyTransition{}, &decoySessionSeed{}}
	migrate = append(migrate, heuristicsTables...) // durable heuristics (also migrated by the writer)
	if err := db.AutoMigrate(migrate...); err != nil {
		return nil, fmt.Errorf("can't create list tables: %w", err)
	}

	// Open the read-only sampling pool AFTER AutoMigrate: mode=ro cannot create
	// tables, so every table a sampler reads (log_entries via the writer, plus the
	// decoy/corpus/class/session tables above) must already exist.
	ro, err := openReadOnlyPool(sqlitePath, maxDecoyReaders)
	if err != nil {
		return nil, err
	}

	s := &DecoySource{
		db: db, ro: ro, rnd: rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec // noise timing, not crypto
		blWarm: make(chan struct{}, 1), blWarmStop: make(chan struct{}), blWarmDone: make(chan struct{}),
		classStop: make(chan struct{}), classDone: make(chan struct{}),
	}

	if err := s.loadMaxRowid(); err != nil {
		return nil, err
	}

	if err := s.loadBlMaxRowid(); err != nil {
		return nil, err
	}

	// Warm BlocklistCategories in the background so the first Blocking-tab visit after
	// a reboot is already hot, and re-warm on every invalidation (see blCatsWarmLoop).
	go s.blCatsWarmLoop()
	s.signalBlWarm()

	// Warm the client_name→device_key overlay off the request path so the first
	// /clients, /people or /clients/classes visit after boot serves from cache and
	// never runs the windowed log_entries GROUP BY inline. Re-warmed on the class
	// tick and after a name/person write. Via kickDominantFP: EVERY refresh rides
	// the single-flight CAS + failure backoff, never a bare goroutine.
	s.kickDominantFP()

	// Class scorer: advance durable device classes from the accumulator on a timer,
	// independent of any UI visit (heuristics.go).
	go s.classScorerLoop()

	return s, nil
}

// stopClassScorer halts the background class-scorer goroutine and waits for it
// to exit. Idempotent: called from Close, and early by tests that need s.db
// single-threaded (logger capture) while the source stays open.
func (s *DecoySource) stopClassScorer() {
	s.classStopOnce.Do(func() { close(s.classStop) })
	<-s.classDone
}

func (s *DecoySource) Close() error {
	// Stop the warmer and wait for it to exit BEFORE closing the ro pool it queries,
	// so a mid-flight recompute can never race the pool close.
	close(s.blWarmStop)
	<-s.blWarmDone

	// Stop the class scorer (uses s.db) before closing the writer connection below.
	s.stopClassScorer()

	if s.ro != nil {
		if roDB, err := s.ro.DB(); err == nil {
			_ = roDB.Close()
		}
	}

	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}

	return sqlDB.Close()
}

// leRowidBounds returns the cached [min,max] rowid range of log_entries for
// indexed random-rowid sampling, refreshing it on leRowidTTL. ok=false when the
// table is empty (cold start). The MIN/MAX probes run on the RO pool and are
// index-backed on the integer primary key.
func (s *DecoySource) leRowidBounds() (minRowid, maxRowid int64, ok bool, err error) {
	s.mu.Lock()
	if !s.leRowidAt.IsZero() && time.Since(s.leRowidAt) < leRowidTTL {
		minRowid, maxRowid = s.leMinRowid, s.leMaxRowid
		s.mu.Unlock()

		return minRowid, maxRowid, maxRowid > 0, nil
	}
	s.mu.Unlock()

	// Two single-aggregate probes: sqlite optimizes MIN(rowid)/MAX(rowid) to a
	// b-tree edge lookup each (a combined SELECT MIN,MAX can full-scan instead).
	if err := s.ro.Raw("SELECT COALESCE(MIN(rowid),0) FROM log_entries").Scan(&minRowid).Error; err != nil {
		return 0, 0, false, err
	}

	if err := s.ro.Raw("SELECT COALESCE(MAX(rowid),0) FROM log_entries").Scan(&maxRowid).Error; err != nil {
		return 0, 0, false, err
	}

	s.mu.Lock()
	s.leMinRowid, s.leMaxRowid, s.leRowidAt = minRowid, maxRowid, time.Now()
	s.mu.Unlock()

	return minRowid, maxRowid, maxRowid > 0, nil
}

// randRowid draws a uniform rowid in [minRowid,maxRowid].
func (s *DecoySource) randRowid(minRowid, maxRowid int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	if maxRowid <= minRowid {
		return minRowid
	}

	return minRowid + s.rnd.Int63n(maxRowid-minRowid+1)
}

// seekRandomReal runs one gap-tolerant random-rowid sample over log_entries. seek
// must be a `... WHERE rowid >= ? AND <preds> ORDER BY rowid LIMIT 1` statement
// whose FIRST bind is the rowid floor; args supplies the remaining binds. It
// draws a uniform floor and takes the first matching row at or after it (walking
// past pruned gaps and interleaved decoy=1 rows), resampling a few times when a
// draw lands in an all-filtered tail, then scanning once from the floor so an
// existing row is never missed. found reports whether any row matched; dest is a
// gorm scan target (populated only when found).
func (s *DecoySource) seekRandomReal(seek string, args []any, dest any) (found bool, err error) {
	minRowid, maxRowid, ok, err := s.leRowidBounds()
	if err != nil || !ok {
		return false, err
	}

	for range rowidSeekTries {
		k := s.randRowid(minRowid, maxRowid)

		res := s.ro.Raw(seek, append([]any{k}, args...)...).Scan(dest)
		if res.Error != nil {
			return false, s.warnSampleErr("seekRandomReal", res.Error)
		}

		if res.RowsAffected > 0 {
			return true, nil
		}
	}

	// All draws landed in filtered tails; scan forward from the floor once.
	res := s.ro.Raw(seek, append([]any{minRowid}, args...)...).Scan(dest)
	if res.Error != nil {
		return false, s.warnSampleErr("seekRandomReal", res.Error)
	}

	return res.RowsAffected > 0, nil
}

func (s *DecoySource) loadMaxRowid() error {
	// Scan into a local, assign under s.mu (mirrors loadBlMaxRowid): the emit
	// workers read maxRowid under the lock, and an unlocked int64 store tears on
	// 32-bit ARM.
	var max int64
	if err := s.db.Raw("SELECT COALESCE(MAX(rowid),0) FROM decoy_domains").Scan(&max).Error; err != nil {
		return err
	}

	s.mu.Lock()
	s.maxRowid = max
	s.mu.Unlock()

	return nil
}

// SeedIfEmpty streams normalized domains (one per line, already decompressed)
// into decoy_domains, but only when the table is empty. Blank lines are
// skipped. Returns the number of rows inserted (0 when already seeded).
func (s *DecoySource) SeedIfEmpty(r io.Reader) (int, error) {
	s.mu.Lock()
	seeded := s.maxRowid > 0
	s.mu.Unlock()

	if seeded {
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
	for range blockResampleTries {
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
	err := s.ro.Raw("SELECT domain FROM decoy_domains WHERE rowid >= ? ORDER BY rowid LIMIT 1", k).Scan(&domain).Error

	return domain, s.warnSampleErr("sampleList", err)
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

	err := s.ro.Raw("SELECT 1 FROM blocklist_domains WHERE domain = ? LIMIT 1", domain).Scan(&one).Error
	if err != nil {
		return false, s.warnSampleErr("isBlockedDomain", err)
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

	since := time.Now().Add(-decoyReplayWindow).UTC() // UTC bind: request_ts is stored UTC, compared lexically

	var rows []struct {
		Hour time.Time `gorm:"column:hour"`
		Cnt  int64     `gorm:"column:cnt"`
	}

	if err := s.ro.Raw(`SELECT hour, cnt FROM agg_hourly WHERE hour >= ?`, since.UTC()).Scan(&rows).Error; err != nil {
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
	since := time.Now().Add(-decoyReplayWindow).UTC() // UTC bind: request_ts is stored UTC, compared lexically

	var row fpRow

	// Uniform-over-rows sampling via an indexed random-rowid seek (rowid is
	// monotonic with request_ts), replacing an ORDER BY RANDOM() full scan.
	_, err := s.seekRandomReal(`SELECT question_type, edns_udp_size, edns_opt_codes, fp_detail FROM log_entries
		WHERE rowid >= ? AND decoy = 0 AND question_name <> '' AND request_ts >= ?
		ORDER BY rowid LIMIT 1`, []any{since}, &row)
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

	// idx_log_entries_etldp_ts (effective_tldp, request_ts) makes this an indexed
	// lookup + reverse-ordered LIMIT 1 instead of a full effective_tldp scan.
	err := s.ro.Raw(`SELECT question_type, edns_udp_size, edns_opt_codes, fp_detail FROM log_entries
		WHERE decoy = 0 AND effective_tldp = ?
		ORDER BY request_ts DESC LIMIT 1`, etldp).Scan(&row).Error
	if err != nil {
		return FpSample{}, s.warnSampleErr("sampleFingerprintForName", err)
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
	IP   string
	Name string // client_name of the sampled row (display / people rollup)
	Key  string // STABLE device_key (fp_hash, else name) — what association rows key on
	Fp   FpSample
}

// SampleClient returns a random recent real client (its IP + the fingerprint of
// one of its rows). Empty IP at cold start (no real rows yet). One indexed random
// row read — same cost profile as SampleRealFingerprint.
func (s *DecoySource) SampleClient() (ClientPersona, error) {
	since := time.Now().Add(-decoyReplayWindow).UTC() // UTC bind: request_ts is stored UTC, compared lexically

	var row struct {
		fpRow

		ClientIP   string `gorm:"column:client_ip"`
		ClientName string `gorm:"column:client_name"`
		FpHash     string `gorm:"column:fp_hash"`
	}

	_, err := s.seekRandomReal(`SELECT client_ip, client_name, fp_hash, question_type, edns_udp_size, edns_opt_codes, fp_detail
		FROM log_entries
		WHERE rowid >= ? AND decoy = 0 AND client_ip <> '' AND request_ts >= ?
		ORDER BY rowid LIMIT 1`, []any{since}, &row)
	if err != nil {
		return ClientPersona{}, err
	}

	// device_key = fp_hash when present (IP-independent), else the name (legacy).
	key := row.FpHash
	if key == "" {
		key = row.ClientName
	}

	return ClientPersona{IP: row.ClientIP, Name: row.ClientName, Key: key, Fp: row.toFpSample()}, nil
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
	since := time.Now().Add(-decoyReplayWindow).UTC() // UTC bind: request_ts is stored UTC, compared lexically

	// limit independent indexed random-rowid draws, each keeping the
	// never-emit-blocked anti-join in its WHERE (per-row, so no resample loop is
	// needed for blocking). Independent draws are at least as uniform as one
	// scan+sort; a draw that finds nothing (cold start / all-filtered) is skipped.
	out := make([]RealQuery, 0, limit)
	seek := `SELECT question_name, question_type FROM log_entries
		WHERE rowid >= ? AND decoy = 0 AND question_name <> '' AND request_ts >= ?
		AND NOT EXISTS (SELECT 1 FROM blocklist_domains b WHERE b.domain = log_entries.question_name)
		ORDER BY rowid LIMIT 1`

	for range limit {
		var q RealQuery

		found, err := s.seekRandomReal(seek, []any{since}, &q)
		if err != nil {
			return out, err
		}

		if !found {
			break // cold start / nothing matches; further draws won't either
		}

		out = append(out, q)
	}

	return out, nil
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
	for range blockResampleTries {
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
	if err := s.ro.Raw("SELECT COALESCE(MAX(rowid),0) FROM noise_corpus").Scan(&max).Error; err != nil {
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

	err := s.ro.Raw("SELECT domain, hits FROM noise_corpus WHERE rowid >= ? ORDER BY rowid LIMIT 1", k).Scan(&row).Error

	return row.Domain, row.Hits, s.warnSampleErr("sampleCorpus", err)
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

	now := time.Now().UTC() // UTC: last_seen is MAX()'d lexically against the writer's UTC RequestTS

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
	since := time.Now().Add(-decoyReplayWindow).UTC() // UTC bind: request_ts is stored UTC, compared lexically

	var seed struct {
		ClientName string    `gorm:"column:client_name"`
		RequestTS  time.Time `gorm:"column:request_ts"`
	}

	// Seed on an indexed random-rowid real row (was ORDER BY RANDOM()); the gather
	// below still rides idx_client_name_request_ts, so the ±window burst is identical.
	found, err := s.seekRandomReal(`SELECT client_name, request_ts FROM log_entries
		WHERE rowid >= ? AND decoy = 0 AND question_name <> '' AND request_ts >= ?
		ORDER BY rowid LIMIT 1`, []any{since}, &seed)
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, nil // cold start
	}

	var rows []cohortRow

	// LIMIT caps the burst: a recorded query storm would otherwise replay upstream
	// as a recurring unbounded decoy burst (every member is re-emitted).
	err = s.ro.Raw(`SELECT question_name, question_type, response_type, request_ts FROM log_entries
		WHERE decoy = 0 AND question_name <> '' AND client_name = ?
		AND request_ts BETWEEN ? AND ?
		ORDER BY request_ts ASC LIMIT ?`,
		seed.ClientName, seed.RequestTS.Add(-cohortWindow), seed.RequestTS.Add(cohortWindow),
		cohortMaxMembers).Scan(&rows).Error
	if err != nil {
		return nil, s.warnSampleErr("sampleCohort", err)
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

// decoyTransition is a materialized Markov transition cur→nxt (eTLD+1) with its
// count over the recent window, rebuilt by refreshSessionModels on the class
// refresh timer so NextInSession is a cheap keyed table read instead of a
// per-emit windowed full scan of log_entries.
type decoyTransition struct {
	Cur string `gorm:"column:cur;primaryKey"`
	Nxt string `gorm:"column:nxt;primaryKey"`
	Cnt int64  `gorm:"column:cnt"`
}

func (decoyTransition) TableName() string { return "decoy_transitions" }

// decoySessionSeed is a materialized session-starting primary (eTLD+1) with its
// count, rebuilt alongside decoyTransition. SessionSeed reads this small table.
type decoySessionSeed struct {
	Cur string `gorm:"column:cur;primaryKey"`
	Cnt int64  `gorm:"column:cnt"`
}

func (decoySessionSeed) TableName() string { return "decoy_session_seeds" }

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
// The transition counts are precomputed into decoy_transitions by
// refreshSessionModels (on the class-refresh timer); this read is a keyed
// primary-key lookup + an in-Go weighted draw. Distribution is identical to the
// former per-emit windowed scan; only freshness moves from live to minutes-stale
// (fine for noise).
func (s *DecoySource) NextInSession(primaryDomain string) (string, error) {
	if primaryDomain == "" {
		return "", nil
	}

	var rows []struct {
		Domain string `gorm:"column:nxt"`
		Cnt    int64  `gorm:"column:cnt"`
	}

	err := s.ro.Raw(`SELECT nxt, cnt FROM decoy_transitions WHERE cur = ?`, primaryDomain).Scan(&rows).Error
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
// cold start. Reads the precomputed decoy_session_seeds table (rebuilt by
// refreshSessionModels), not a per-emit windowed scan.
func (s *DecoySource) SessionSeed() (string, error) {
	var rows []struct {
		Domain string `gorm:"column:cur"`
		Cnt    int64  `gorm:"column:cnt"`
	}

	err := s.ro.Raw(`SELECT cur, cnt FROM decoy_session_seeds`).Scan(&rows).Error
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

	since := time.Now().Add(-decoyReplayWindow).UTC() // UTC bind: request_ts is stored UTC, compared lexically

	var ts []time.Time

	// idx_log_entries_etldp_ts (effective_tldp, request_ts) serves both the
	// effective_tldp match and the request_ts range+order, so this no longer scans.
	err := s.ro.Raw(`SELECT request_ts FROM log_entries
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

	slices.Sort(deltas)

	return deltas[len(deltas)/2], true // lower-median; noise cadence, not statistics
}

// --- device-class classification (cached) -----------------------------------

// Device classes a client can be assigned. "unknown" covers too-little-data.
// The vendor-tell classes (game-console … printer) are only split when a strong
// single-vendor telemetry share is present (see classify); everything else with
// broad, browser-like behaviour stays "workstation" and low-diversity beacons
// stay "iot". Classes the map flagged as DNS-inseparable (phone vs tablet,
// desktop vs laptop, generic no-vendor IoT) are deliberately NOT split.
const (
	ClassIoT          = "iot"
	ClassWorkstation  = "workstation"
	ClassServer       = "server"
	ClassMobile       = "mobile"        // push-endpoint + broad, bursty app traffic
	ClassTVStreaming  = "tv-streaming"  // streaming-CDN dominated
	ClassGameConsole  = "game-console"  // xbox/playstation/nintendo backends
	ClassSmartSpeaker = "smart-speaker" // sonos / alexa voice service
	ClassCamera       = "camera"        // hik/axis/reolink/wyze/ring
	ClassPrinter      = "printer"       // hp/epson/canon/brother cloud print

	// Vendor-tell classes (all reliably DNS-separable by a dedicated single-vendor
	// cloud no generic PC/phone hits). Behaviour only GATES these, never identifies.
	ClassNAS          = "nas"                 // synology / qnap appliance clouds
	ClassRouterInfra  = "router-infra"        // ubiquiti / netgear / asus gateway+AP
	ClassMediaServer  = "media-server"        // plex / emby HTPC
	ClassSmartTV      = "smart-tv"            // LG webOS / Samsung Tizen platform telemetry
	ClassSmartHomeHub = "smart-home-hub"      // smartthings / hubitat / homey bridge
	ClassThermostat   = "thermostat-climate"  // nest / ecobee / honeywell
	ClassLighting     = "smart-lighting-plug" // hue / kasa / tp-link
	ClassWearable     = "wearable"            // fitbit / garmin (NOT apple watch)

	ClassUnknown = "unknown"
)

// validClass reports whether c is an assignable device class (empty = clear override).
func validClass(c string) bool {
	switch c {
	case ClassIoT, ClassWorkstation, ClassServer,
		ClassMobile, ClassTVStreaming, ClassGameConsole, ClassSmartSpeaker, ClassCamera, ClassPrinter,
		ClassNAS, ClassRouterInfra, ClassMediaServer, ClassSmartTV, ClassSmartHomeHub,
		ClassThermostat, ClassLighting, ClassWearable,
		ClassUnknown:
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

	// Vendor-tell shares: minimum fraction of a cohort's queries hitting a
	// vendor's telemetry backends before that class is assigned. HIGH-confidence
	// single-vendor signals, so the bars are low — a device that beacons a console
	// backend at all is almost certainly that console. printer is gated on low
	// diversity too (a workstation that pings HP once must not become a printer).
	// ponytail: fixed shares; tune per fleet if a class over/under-fires.
	classGameShare    = 0.15
	classCameraShare  = 0.15
	classSpeakerShare = 0.15
	classPrinterShare = 0.10
	classStreamShare  = 0.30

	// New vendor-tell shares. Domains are unique to one vendor cloud, so the bars
	// are low and false-positive-safe (a workstation won't hit tplinkcloud.com at
	// 12%). Low-volume named-IoT subtypes get higher bars (their few queries mostly
	// go to their own cloud). ponytail: fixed shares; tune per fleet.
	classNASShare        = 0.05 // quickconnect/myqnapcloud relay + update polling: small but persistent
	classRouterShare     = 0.05 // vendor firmware/DDNS: small slice of a forwarder
	classMediaShare      = 0.08 // plex clients poll plex.tv steadily
	classSmartTVShare    = 0.08 // OEM platform telemetry as a fraction of a mostly-streaming set
	classHubShare        = 0.10 // hub-cloud keepalive
	classThermostatShare = 0.10 // periodic small set
	classLightingShare   = 0.12 // tiny device, most queries to its own cloud
	classWearableShare   = 0.10 // low-volume periodic health sync
)

// Vendor-telemetry signal sets for the extra classes. Matched as question_name
// LIKE '%pattern%' (substring), same shape as serverNameLikes. Kept small and
// HIGH-confidence — a seed, not an exhaustive registry. Deliberately avoid broad
// app CDNs (spotify, steam) that a phone/PC also hits.
//
//nolint:gochecknoglobals // static signal tables
var (
	gameSignals    = []string{"xboxlive.com", "playstation.net", "nintendo.net"}
	cameraSignals  = []string{"hik-connect.com", "axis.com", "reolink.com", "wyzecam.com", "ring.com", "ezvizlife.com"}
	speakerSignals = []string{"sonos.com", "avs-alexa", "alexa.amazon"}
	printerSignals = []string{"hpeprint", "hpconnected", "epsonconnect", "brother.com", "canon.com", "bjnp"}
	streamSignals  = []string{"nflxvideo.net", "googlevideo.com", "roku.com", "amazonvideo.com", "ttvnw.net", "hulustream.com"}
	pushSignals    = []string{"push.apple.com", "mtalk.google.com"} // OS push = interactive personal device

	// New vendor-tell families (see the +8 taxonomy). Each is a dedicated single-
	// vendor cloud no other class or a generic PC/phone hits — the only reliably
	// DNS-separable axis. Deliberately NOT NTP (ambiguous, stays in serverEtldps)
	// and NOT streaming CDNs (a smart-TV's OEM telemetry, not its stream, is the tell).
	nasSignals = []string{"quickconnect.to", "synology.com", "synologydns.com", "myqnapcloud.com", "qnapcloud.com", "qnap.com"}
	// ".ui.com" (leading dot) not "ui.com": the bare token stole ennui.com/sui.com/
	// gui.company.com. Dropped bare "unifi" (matched unifiedlayer.com — Bluehost/EIG
	// hosting) — Ubiquiti's real domains are under .ui.com/ubnt.com. Dropped "dyndns":
	// DDNS is used by cameras/NAS/DVRs for remote access, not router-specific, so it
	// stole those into router-infra.
	routerSignals     = []string{".ui.com", "ubnt.com", "asuscomm.com", "routerlogin.net", "netgear.com"}
	mediaSignals      = []string{"plex.tv", "plex.direct", "plexapp.com", "emby.media", "mb3admin.com"}
	smartTVSignals    = []string{"lgtvsdp.com", "lgtvcommon.com", "lgsmartplatform", "ngfts.lge.com", "samsungcloudsolution.com", "samsungotn.net", "samsungtvns.net", "samsungacr.com"}
	hubSignals        = []string{"smartthings.com", "hubitat.com", "athom.com", "homey.app"}
	thermostatSignals = []string{"nest.com", "ecobee.com", "honeywellhome.com", "mytotalconnectcomfort.com"}
	lightingSignals   = []string{"meethue.com", "philips-hue", "tplinkcloud.com", "tplinkra.com", "kasaapp"}
	wearableSignals   = []string{"fitbit.com", "garmin.com", "gcs.garmin.com", "services.garmin.com", "whoop.com", "polar.com"}
)

// deviceKeyExpr is the SQL for a row's STABLE device identity: its fp_hash (wire
// software fields only — IP-independent by construction, see model.Fingerprint.
// Hash) when present, else the client_name (legacy / fp-less rows keep the old
// name keying so nothing regresses). This is the key every association table is
// stored under, so a DHCP lease change / roam — which changes client_ip and the
// IP-fallback client_name but NOT the fingerprint — keeps the same key.
const deviceKeyExpr = "CASE WHEN fp_hash <> '' THEN fp_hash ELSE client_name END"

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

// classFeatures is the per-device-key aggregate the classifier scores. Client
// holds the STABLE device_key (fp_hash, else client_name — see deviceKeyExpr).
type classFeatures struct {
	Client      string `gorm:"column:c"`
	N           int64  `gorm:"column:n"`
	Domains     int64  `gorm:"column:domains"`
	Qtypes      int64  `gorm:"column:qtypes"`
	ServerHits  int64  `gorm:"column:serverhits"`
	GameHits    int64  `gorm:"column:gamehits"`
	CameraHits  int64  `gorm:"column:camerahits"`
	SpeakerHits int64  `gorm:"column:speakerhits"`
	PrinterHits int64  `gorm:"column:printerhits"`
	StreamHits  int64  `gorm:"column:streamhits"`
	PushHits    int64  `gorm:"column:pushhits"`
	// +8 vendor-tell classes
	NASHits        int64   `gorm:"column:nashits"`
	RouterHits     int64   `gorm:"column:routerhits"`
	MediaHits      int64   `gorm:"column:mediahits"`
	SmartTVHits    int64   `gorm:"column:smarttvhits"`
	HubHits        int64   `gorm:"column:hubhits"`
	ThermostatHits int64   `gorm:"column:thermostathits"`
	LightingHits   int64   `gorm:"column:lightinghits"`
	WearableHits   int64   `gorm:"column:wearablehits"`
	MeanGap        float64 `gorm:"column:meangap"`
	MeanGap2       float64 `gorm:"column:meangap2"`
}

// classify scores one device-key's features into a device class. Vendor-tell
// classes (single-vendor telemetry share) are checked before the generic
// behavioural fallbacks, since a console/camera/printer backend hit is far more
// specific than "broad → workstation" or "narrow → iot".
func (f classFeatures) classify() string {
	if f.N < classMinSamples {
		return ClassUnknown
	}

	share := func(hits int64) float64 { return float64(hits) / float64(f.N) }

	switch {
	// nas/router/media BEFORE server: synology/asus/netgear firmware + a plex host's
	// OS updates all hit serverNameLikes ("update.", "-update"); the vendor tell is
	// more specific and must win, else a Synology scores "server".
	case share(f.NASHits) >= classNASShare:
		return ClassNAS
	case share(f.RouterHits) >= classRouterShare:
		return ClassRouterInfra
	// media-server ALSO requires low domain diversity (printer-style gate): plex.tv/
	// plex.direct are hit by Plex CLIENTS too, so a broad browsing PC used for Plex
	// viewing would otherwise be stolen from workstation. A real HTPC appliance is
	// narrow (≤ classIoTMaxDomains); a workstation is broad (> it) — that diversity is
	// the only axis separating them, since the domains themselves don't.
	case share(f.MediaHits) >= classMediaShare && f.Domains <= classIoTMaxDomains:
		return ClassMediaServer
	case share(f.ServerHits) >= classServerShare:
		return ClassServer
	case share(f.GameHits) >= classGameShare:
		return ClassGameConsole
	case share(f.CameraHits) >= classCameraShare:
		return ClassCamera
	// smart-home-hub / speaker / low-volume named-IoT subtypes: dedicated vendor
	// clouds, disjoint signals, relative order free among themselves.
	case share(f.HubHits) >= classHubShare:
		return ClassSmartHomeHub
	case share(f.SpeakerHits) >= classSpeakerShare:
		return ClassSmartSpeaker
	case share(f.ThermostatHits) >= classThermostatShare:
		return ClassThermostat
	case share(f.LightingHits) >= classLightingShare:
		return ClassLighting
	case share(f.WearableHits) >= classWearableShare:
		return ClassWearable
	// printer: distinctive but low-volume, so also require low diversity — a
	// workstation that pings a print-cloud once must not flip to printer.
	case share(f.PrinterHits) >= classPrinterShare && f.Domains <= classIoTMaxDomains:
		return ClassPrinter
	// smart-tv BEFORE stream: a smart-TV also hits streamSignals; the OEM platform
	// telemetry is the more specific tell (a pure Roku/FireTV stick has none →
	// correctly stays tv-streaming).
	case share(f.SmartTVHits) >= classSmartTVShare:
		return ClassSmartTV
	case share(f.StreamHits) >= classStreamShare:
		return ClassTVStreaming
	// mobile: an OS push endpoint (interactive personal device) plus broad
	// browsing breadth — distinguishes a phone from a narrow-beacon IoT device.
	case f.PushHits > 0 && f.Domains > classIoTMaxDomains:
		return ClassMobile
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

// RefreshClientClasses scores every device's auto class from the DURABLE
// device_class_signal accumulator (folded at write time, purge-immune) instead of
// re-scanning log_entries — so a "clear all logs" no longer reverts every class to
// unknown (the bug this layer fixes). Now a manual "score now" nudge over the same
// accumulator the live ticker reads; the override column is preserved. Call on a
// timer / first use — NOT per emission (single connection, ~110 devices).
func (s *DecoySource) RefreshClientClasses() error {
	if err := s.scoreDeviceClasses(); err != nil {
		return err
	}

	since := time.Now().Add(-decoyReplayWindow).UTC() // UTC bind: request_ts is stored UTC, compared lexically

	// Re-key the manual overrides (name, person, class override) from the legacy
	// client_name key to the stable fp device_key. The auto class above rebuilds
	// itself under the new key, but the manual `override` column does not — nor do
	// the identity/person tables — so they must be migrated explicitly (best-effort,
	// idempotent, off the request path — see migrateLegacyKeys). Runs on this
	// throttled timer, never at boot.
	s.migrateLegacyKeys(since)

	// Same timer, same recent window: rematerialize the Markov session models so
	// NextInSession/SessionSeed read cheap tables instead of scanning per emit.
	return s.refreshSessionModels(since)
}

// roUI returns the read-only handle bound to a bounded-deadline context so a UI
// read on an exhausted/slow pool fails fast instead of hanging on pool acquisition
// (see uiReadTimeout). The caller MUST defer the returned cancel. Emit-path
// samplers do NOT use this — they keep context.Background() on their dedicated conns.
func (s *DecoySource) roUI() (*gorm.DB, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), uiReadTimeout)

	return s.ro.WithContext(ctx), cancel
}

// dominantFPByNameCached serves the cached client_name→device_key overlay so the
// /clients, /people and /clients/classes request paths do ZERO log_entries scans —
// literally zero even at t=0. On a cold cache (only the tiny boot window before the
// startup warm lands, or after a transient scan error) it NEVER computes inline;
// it kicks a single off-path refresh and serves the passthrough-only overlay (nil)
// this once. A cold miss is a delayed secondary-name rollup, not a wrong value:
// fanKeysToNames passes raw device keys through regardless, so names still resolve
// once the warm publishes (~100ms). All real refreshes stay off the request path.
func (s *DecoySource) dominantFPByNameCached() map[string]string {
	s.mu.Lock()
	m, valid := s.fpByName, s.fpByNameValid
	s.mu.Unlock()

	if !valid {
		s.kickDominantFP()
	}

	return m
}

// kickDominantFP launches a single background refreshDominantFP, single-flighted so
// a burst of cold reads spawns one recompute, not one per request (which would pile
// the windowed GROUP BY onto the RO pool). Self-heals a boot-warm scan error without
// waiting for the 5-min class tick, and never blocks the caller.
func (s *DecoySource) kickDominantFP() {
	if time.Now().UnixNano() < s.fpRetryAt.Load() {
		return // recent recompute failure: back off instead of re-kicking a doomed scan
	}

	if s.fpRefreshing.CompareAndSwap(false, true) {
		go func() {
			defer s.fpRefreshing.Store(false)
			s.refreshDominantFP()
		}()
	}
}

// refreshDominantFP recomputes the overlay off the request path (startup warm, the
// 5-min class tick, a background refresh after a name/person write) and publishes a
// FRESH map by pointer swap. A scan error leaves the previous cache untouched (never
// caches nil), so a transient DB error can't blank the overlay for every reader.
func (s *DecoySource) refreshDominantFP() map[string]string {
	since := time.Now().Add(-decoyReplayWindow).UTC()

	m := s.dominantFPByName(since)
	if m == nil {
		s.fpRetryAt.Store(time.Now().Add(fpKickBackoff).UnixNano())

		return nil
	}

	s.fpRetryAt.Store(0)

	s.mu.Lock()
	s.fpByName, s.fpByNameValid = m, true
	s.mu.Unlock()

	return m
}

// dominantFPByName maps every client_name seen in the window to its STABLE
// device_key: the modal (most-frequent) non-empty fp_hash it presented, or the
// name itself when it has no fingerprinted rows. This is the read-side inverse of
// deviceKeyExpr — used to translate a UI-facing client_name into the key its
// association rows are stored under, and to fan a stored key back out to the
// current client_name(s) presenting it (people/name rollups).
//
// ponytail: one windowed GROUP BY over log_entries; NEVER on the request path —
// served through dominantFPByNameCached, refreshed off-path only.
func (s *DecoySource) dominantFPByName(since time.Time) map[string]string {
	var rows []struct {
		Name string `gorm:"column:client_name"`
		FP   string `gorm:"column:fp_hash"`
		Cnt  int64  `gorm:"column:cnt"`
	}

	// Background budget, NOT roUI: every caller is an off-request refresher, and
	// the 5s request budget makes a large-log recompute permanently fail (raw
	// hashes in the UI forever, legacy-key migration no-oping).
	ctx, cancel := context.WithTimeout(context.Background(), fpRefreshTimeout)
	defer cancel()

	err := s.ro.WithContext(ctx).Raw(`SELECT client_name, fp_hash, COUNT(*) AS cnt
		FROM log_entries
		WHERE decoy = 0 AND client_name <> '' AND request_ts >= ?
		GROUP BY client_name, fp_hash`, since).Scan(&rows).Error
	if err != nil {
		log.PrefixedLog("decoy_source").WithError(err).Warn("client_name→device_key overlay scan failed")

		return nil
	}

	best := map[string]int64{}
	out := map[string]string{}

	for _, r := range rows {
		if r.FP == "" {
			if _, ok := out[r.Name]; !ok {
				out[r.Name] = r.Name // fp-less: key on the name (legacy behaviour)
			}

			continue
		}

		if r.Cnt > best[r.Name] {
			best[r.Name] = r.Cnt
			out[r.Name] = r.FP
		}
	}

	return out
}

// deviceKey returns the STABLE device_key for a single client_name: the modal
// non-empty fp_hash it presents over the window, else the name unchanged. This is
// what re-keys class/person/name lookups off client_ip: two IPs of one device
// share a fingerprint, so both resolve to the same key. One indexed query; used by
// the (rare) UI setters and the display-side ClientClass, NOT the emit hot path
// (the engine carries ClientPersona.Key and calls ClientClassByKey directly).
func (s *DecoySource) deviceKey(name string) string {
	if name == "" {
		return name
	}

	since := time.Now().Add(-decoyReplayWindow).UTC() // UTC bind: request_ts stored UTC, compared lexically

	var fp string

	ro, cancel := s.roUI()
	defer cancel()

	// idx_client_name_request_ts pins the client prefix; GROUP BY fp_hash sorts the
	// (tiny) per-client subset. Error / no fp → fall back to the name.
	_ = ro.Raw(`SELECT fp_hash FROM log_entries INDEXED BY idx_client_name_request_ts
		WHERE decoy = 0 AND client_name = ? AND fp_hash <> '' AND request_ts >= ?
		GROUP BY fp_hash ORDER BY COUNT(*) DESC LIMIT 1`, name, since).Scan(&fp).Error

	if fp == "" {
		return name
	}

	return fp
}

// migrateLegacyKeys re-points manual overrides (display name, person mapping,
// class override) from the old client_name key onto the stable fp device_key, so
// a user's "Alex's iPhone → Alex" mapping and any manual class survive the
// re-keying instead of being silently orphaned. The AUTO class rebuilds itself
// under the new key on every refresh; the manual `override` column does NOT, so
// it is migrated here alongside the other irreplaceable manual tables.
//
// Pi3-safe: bounded to the ~fleet-size set of names with a fp, plain indexed
// upsert/delete on the writer handle, on the throttled refresh timer — NEVER a
// blocking boot op. Idempotent: once a row is fp-keyed its key is no longer a
// name in nameToKey, so it is skipped.
func (s *DecoySource) migrateLegacyKeys(since time.Time) {
	nameToKey := s.dominantFPByName(since)

	for name, key := range nameToKey {
		if key == name { // fp-less name: nothing stable to re-key onto
			continue
		}

		// Move the name-keyed manual rows onto the fp key without clobbering a row
		// already stored there (INSERT OR IGNORE new key, then drop the old name row).
		// Each copy+delete pair runs in ONE transaction with errors propagated: a
		// SQLITE_BUSY on the copy must roll the DELETE back too, or the manual row
		// is permanently lost. A failed pair is retried on the next refresh tick.
		err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(`INSERT OR IGNORE INTO client_identity (client, name, updated_at)
				SELECT ?, name, updated_at FROM client_identity WHERE client = ?`, key, name).Error; err != nil {
				return err
			}

			return tx.Exec(`DELETE FROM client_identity WHERE client = ?`, name).Error
		})
		if err == nil {
			err = s.db.Transaction(func(tx *gorm.DB) error {
				if err := tx.Exec(`INSERT OR IGNORE INTO client_person (client, person, updated_at)
					SELECT ?, person, updated_at FROM client_person WHERE client = ?`, key, name).Error; err != nil {
					return err
				}

				return tx.Exec(`DELETE FROM client_person WHERE client = ?`, name).Error
			})
		}

		if err == nil {
			// class override: the fp-keyed row already exists (RefreshClientClasses ran
			// first with its auto class), so INSERT OR IGNORE alone can't carry the
			// override. Adopt the legacy override onto the fp row only when that row has
			// none of its own (never clobber a newer manual choice), then drop the stale
			// name-keyed row so it can't shadow-revert on the next read.
			err = s.db.Transaction(func(tx *gorm.DB) error {
				if err := tx.Exec(`INSERT OR IGNORE INTO client_class (client, class, override, updated_at)
					SELECT ?, class, override, updated_at FROM client_class WHERE client = ?`, key, name).Error; err != nil {
					return err
				}

				if err := tx.Exec(`UPDATE client_class
					SET override = (SELECT override FROM client_class WHERE client = ?)
					WHERE client = ? AND COALESCE(override,'') = ''
					  AND COALESCE((SELECT override FROM client_class WHERE client = ?),'') <> ''`,
					name, key, name).Error; err != nil {
					return err
				}

				return tx.Exec(`DELETE FROM client_class WHERE client = ?`, name).Error
			})
		}

		if err != nil {
			log.PrefixedLog("decoy_source").WithField("name", name).WithError(err).
				Warn("legacy-key migration deferred (will retry next refresh)")
		}
	}
}

// refreshSessionModels rebuilds the decoy_transitions and decoy_session_seeds
// tables from the same recent window RefreshClientClasses scans, lifting the
// per-emit windowed scans out of NextInSession/SessionSeed (Cat IV). Runs on the
// class-refresh timer (off the emit hot path), on the pinned-1 writer handle.
//
// ponytail: two extra windowed scans of log_entries on the refresh timer rather
// than folding all three aggregates into one pass (they GROUP BY different keys,
// so a single SELECT can't express them and a shared temp table isn't worth the
// machinery). The timer is throttled and off-request; the emit path is what this
// fix unblocks.
func (s *DecoySource) refreshSessionModels(since time.Time) error {
	gapSecs := sessionGap.Seconds()

	// ONE transaction for both DELETE+INSERT pairs: as separate autocommits, an
	// INSERT failure (SQLITE_BUSY) left the just-DELETEd model table empty until
	// the next refresh — the rollback keeps the previous model instead.
	return s.db.Transaction(func(tx *gorm.DB) error {
		// Transitions: cur→nxt same-session hop counts (mirrors the former
		// NextInSession subquery, minus the per-call cur=? filter).
		if err := tx.Exec(`DELETE FROM decoy_transitions`).Error; err != nil {
			return err
		}

		err := tx.Exec(`INSERT INTO decoy_transitions (cur, nxt, cnt)
			SELECT cur, nxt, COUNT(*) AS cnt FROM (
				SELECT effective_tldp AS cur,
				       LEAD(effective_tldp) OVER w AS nxt,
				       (julianday(LEAD(request_ts) OVER w) - julianday(request_ts))*86400 AS gap
				FROM log_entries
				WHERE decoy = 0 AND effective_tldp <> '' AND request_ts >= ?
				WINDOW w AS (PARTITION BY client_name ORDER BY request_ts)
			)
			WHERE nxt IS NOT NULL AND nxt <> cur AND gap >= 0 AND gap <= ?
			GROUP BY cur, nxt`, since, gapSecs).Error
		if err != nil {
			return err
		}

		// Session seeds: first-of-session primary counts (mirrors the former
		// SessionSeed subquery).
		if err := tx.Exec(`DELETE FROM decoy_session_seeds`).Error; err != nil {
			return err
		}

		return tx.Exec(`INSERT INTO decoy_session_seeds (cur, cnt)
			SELECT cur, COUNT(*) AS cnt FROM (
				SELECT effective_tldp AS cur,
				       (julianday(request_ts) - julianday(LAG(request_ts) OVER w))*86400 AS gap
				FROM log_entries
				WHERE decoy = 0 AND effective_tldp <> '' AND request_ts >= ?
				WINDOW w AS (PARTITION BY client_name ORDER BY request_ts)
			)
			WHERE gap IS NULL OR gap > ?
			GROUP BY cur`, since, gapSecs).Error
	})
}

// ClientClassByKey returns the cached effective class for a STABLE device_key
// (override if set, else the auto class), or ClassUnknown when the key has no
// cached row yet. Fast: one primary-key lookup, no scan — this is the emit
// hot-path entry (the engine passes ClientPersona.Key, already IP-independent).
func (s *DecoySource) ClientClassByKey(key string) (string, error) {
	var row clientClass

	err := s.ro.Raw("SELECT class, override FROM client_class WHERE client = ? LIMIT 1", key).Scan(&row).Error
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

// ClientClass returns the effective class for a client_name (display / UI /
// tests). It first resolves the name to its stable device_key, so it stays
// correct across an IP change, then does the same cheap lookup as
// ClientClassByKey. One extra indexed query for the translation — fine off the
// emit hot path (which uses ClientClassByKey directly).
func (s *DecoySource) ClientClass(client string) (string, error) {
	return s.ClientClassByKey(s.deviceKey(client))
}

// ListClientClasses returns every cached client classification (auto + override +
// resolved effective), for the management UI.
func (s *DecoySource) ListClientClasses() ([]ClientClassInfo, error) {
	ro, cancel := s.roUI()
	defer cancel()

	var rows []clientClass
	if err := ro.Raw("SELECT client, class, override, updated_at FROM client_class ORDER BY client").Scan(&rows).Error; err != nil {
		return nil, err
	}

	// device_key → EVERY current client_name presenting it, so the UI shows names
	// (not raw fp hashes) and the name-keyed classOf join downstream resolves for
	// ALL of a device's names. A single lexically-first representative broke a
	// roamed device: its active (non-representative) name missed the join and fell
	// back to unknown. Names still round-trip: a PUT re-resolves to the same key.
	// Keys with no current traffic keep the raw key; sorted for a stable order.
	overlay := s.dominantFPByNameCached()

	names := make([]string, 0, len(overlay))
	for name := range overlay {
		names = append(names, name)
	}

	slices.Sort(names)

	keyToNames := map[string][]string{}

	for _, name := range names {
		key := overlay[name]
		keyToNames[key] = append(keyToNames[key], name)
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

		clients := keyToNames[r.Client]
		if len(clients) == 0 {
			clients = []string{r.Client}
		}

		for _, client := range clients {
			out = append(out, ClientClassInfo{
				Client: client, Class: r.Class, Override: r.Override, Effective: eff, UpdatedAt: r.UpdatedAt,
			})
		}
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
		return errors.New("client must not be empty")
	}

	if class == "auto" {
		class = ""
	}

	if class != "" && !validClass(class) {
		return fmt.Errorf("invalid device class %q", class)
	}

	key := s.deviceKey(client) // store the override under the stable device_key

	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "client"}},
		DoUpdates: clause.Assignments(map[string]any{"override": class}),
	}).Create(&clientClass{Client: key, Override: class, UpdatedAt: time.Now()}).Error
}

// SampleClientOfClass returns a random real client whose EFFECTIVE class matches
// class, as a ClientPersona (IP + fingerprint) for decoy attribution. Empty
// ClientPersona (empty IP) when no such client exists — caller falls back to
// SampleClient. Two cheap indexed random-row reads.
func (s *DecoySource) SampleClientOfClass(class string) (ClientPersona, error) {
	var key string

	// ~110-row client_class table: ORDER BY RANDOM() is negligible, left as-is.
	// `client` is the stable device_key (fp_hash, else client_name).
	err := s.ro.Raw(`SELECT client FROM client_class
		WHERE CASE WHEN override <> '' THEN override ELSE class END = ?
		ORDER BY RANDOM() LIMIT 1`, class).Scan(&key).Error
	if err != nil {
		return ClientPersona{}, err
	}

	if key == "" {
		return ClientPersona{}, nil
	}

	since := time.Now().Add(-decoyReplayWindow).UTC() // UTC bind: request_ts is stored UTC, compared lexically

	// A random real row FOR THIS device_key. The key is a fp_hash (prod / IP-
	// independent case) or a legacy client_name; each rides its OWN single-column
	// index, never idx_decoy_request_ts (the emit hot path must not scan the whole
	// decoy=0 partition — see the #10 index-plan guard). Count then random OFFSET.
	col, idx := "client_name", "idx_client_name_request_ts"
	if looksLikeFPHash(key) {
		col, idx = "fp_hash", "idx_log_entries_fp_hash"
	}

	base := "FROM log_entries INDEXED BY " + idx +
		" WHERE decoy = 0 AND " + col + " = ? AND client_ip <> '' AND request_ts >= ?"

	var cnt int64

	err = s.ro.Raw("SELECT COUNT(*) "+base, key, since).Scan(&cnt).Error
	if err != nil {
		return ClientPersona{}, err
	}

	if cnt == 0 {
		return ClientPersona{}, nil
	}

	s.mu.Lock()
	off := s.rnd.Int63n(cnt)
	s.mu.Unlock()

	var row struct {
		fpRow

		ClientIP   string `gorm:"column:client_ip"`
		ClientName string `gorm:"column:client_name"`
	}

	err = s.ro.Raw("SELECT client_ip, client_name, question_type, edns_udp_size, edns_opt_codes, fp_detail "+
		base+" ORDER BY request_ts LIMIT 1 OFFSET ?", key, since, off).Scan(&row).Error
	if err != nil {
		return ClientPersona{}, err
	}

	return ClientPersona{IP: row.ClientIP, Name: row.ClientName, Key: key, Fp: row.toFpSample()}, nil
}

// looksLikeFPHash reports whether s has the shape of a fp_hash (20 lowercase hex
// chars — hex.EncodeToString of 10 bytes). Used to route a device_key to the
// right single-column index: fp keys → idx_log_entries_fp_hash, everything else
// (IP strings, hostnames) → the client_name index.
//
// ponytail: a 20-char all-lowercase-hex HOSTNAME would misroute, but that never
// occurs in practice; not worth a schema flag.
func looksLikeFPHash(s string) bool {
	if len(s) != 20 {
		return false
	}

	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// --- manual client display-name override (Phase 2) --------------------------

// clientIdentity is the manual display-name override for a client. Keyed on
// client_name like clientClass: a DHCP/rDNS/config rename splits a device's
// stored rows across two keys — an override does NOT re-key history (blueprint
// R6). Copies the clientClass storage shape verbatim.
type clientIdentity struct {
	Client    string `gorm:"column:client;primaryKey"`
	Name      string `gorm:"column:name"`
	UpdatedAt time.Time
}

func (clientIdentity) TableName() string { return "client_identity" }

// clientPerson maps a client to a household member. Registered for AutoMigrate
// only; read/write methods are Phase 5 (opt-in, most sensitive).
type clientPerson struct {
	Client    string `gorm:"column:client;primaryKey"`
	Person    string `gorm:"column:person"`
	UpdatedAt time.Time
}

func (clientPerson) TableName() string { return "client_person" }

// clientProfile is the precomputed presence histogram. Registered for
// AutoMigrate only; the timer-refreshed compute is Phase 3 (opt-in).
type clientProfile struct {
	Client      string `gorm:"column:client;primaryKey"`
	HourHistUTC string `gorm:"column:hour_hist_utc"` // 24-int CSV of active-hour counts, UTC buckets
	FirstSeen   time.Time
	LastSeen    time.Time
	UpdatedAt   time.Time
}

func (clientProfile) TableName() string { return "client_profile" }

// ClientProfileInfo is the decoded presence histogram for one client: queries per
// UTC hour-of-day (0..23) plus the observed activity span. The server localizes
// the histogram with the configured TZ before rendering.
type ClientProfileInfo struct {
	HourHistUTC [24]int
	FirstSeen   time.Time
	LastSeen    time.Time
	UpdatedAt   time.Time
}

// encodeHourHist joins a 24-int histogram into the stored CSV form.
func encodeHourHist(h [24]int) string {
	parts := make([]string, 24)
	for i, v := range h {
		parts[i] = strconv.Itoa(v)
	}

	return strings.Join(parts, ",")
}

// decodeHourHist parses the stored CSV back into a 24-int histogram. A malformed
// or short row yields zeros for the missing slots rather than an error.
func decodeHourHist(s string) [24]int {
	var out [24]int

	for i, tok := range strings.Split(s, ",") {
		if i >= 24 {
			break
		}

		out[i], _ = strconv.Atoi(strings.TrimSpace(tok))
	}

	return out
}

// RefreshClientProfiles recomputes every client's presence histogram from the
// hourly aggregates and upserts client_profile. This is a windowed agg_hourly
// scan + GROUP BY — heavy recompute-all — so it runs ONLY on the throttled
// off-request timer (mirrors RefreshClientClasses), never on the request path.
// Windowed to heuristicsStaleAfter: bounds the scan on hour (the PK prefix) and
// keeps stale months from diluting the "when is this device active" signal.
func (s *DecoySource) RefreshClientProfiles() error {
	var rows []struct {
		Client string    `gorm:"column:client_name"`
		Hour   time.Time `gorm:"column:hour"`
		Cnt    int64     `gorm:"column:cnt"`
	}

	since := time.Now().Add(-heuristicsStaleAfter).UTC() // UTC bind: hour is stored UTC, compared lexically

	err := s.db.Raw(`SELECT client_name, hour, SUM(cnt) AS cnt FROM agg_hourly
		WHERE hour >= ? AND client_name <> '' GROUP BY client_name, hour`, since).Scan(&rows).Error
	if err != nil {
		return err
	}

	type acc struct {
		hist        [24]int
		first, last time.Time
	}

	byClient := map[string]*acc{}

	for _, r := range rows {
		a := byClient[r.Client]
		if a == nil {
			a = &acc{}
			byClient[r.Client] = a
		}

		a.hist[r.Hour.UTC().Hour()] += int(r.Cnt)

		if a.first.IsZero() || r.Hour.Before(a.first) {
			a.first = r.Hour
		}

		if r.Hour.After(a.last) {
			a.last = r.Hour
		}
	}

	now := time.Now()

	for client, a := range byClient {
		row := clientProfile{
			Client:      client,
			HourHistUTC: encodeHourHist(a.hist),
			FirstSeen:   a.first,
			LastSeen:    a.last,
			UpdatedAt:   now,
		}

		err := s.db.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "client"}},
			DoUpdates: clause.Assignments(map[string]any{
				"hour_hist_utc": row.HourHistUTC,
				"first_seen":    row.FirstSeen,
				"last_seen":     row.LastSeen,
				"updated_at":    row.UpdatedAt,
			}),
		}).Create(&row).Error
		if err != nil {
			return err
		}
	}

	return nil
}

// ClientProfile returns the precomputed presence histogram for client (one
// primary-key lookup — cheap, safe on the request path), or a zero-value
// ClientProfileInfo when the client has no profile row yet.
func (s *DecoySource) ClientProfile(client string) (ClientProfileInfo, error) {
	ro, cancel := s.roUI()
	defer cancel()

	var row clientProfile
	if err := ro.Raw(
		"SELECT client, hour_hist_utc, first_seen, last_seen, updated_at FROM client_profile WHERE client = ? LIMIT 1",
		client).Scan(&row).Error; err != nil {
		return ClientProfileInfo{}, err
	}

	return ClientProfileInfo{
		HourHistUTC: decodeHourHist(row.HourHistUTC),
		FirstSeen:   row.FirstSeen,
		LastSeen:    row.LastSeen,
		UpdatedAt:   row.UpdatedAt,
	}, nil
}

// ListProfiles bulk-reads every client's presence profile — ClientProfile with
// the WHERE dropped. A full scan of the tiny one-row-per-client client_profile
// cache table (safe off-request); backs the personas rollup's fleet/per-person
// presence sums. Keyed by client_name (same key ClientProfile looks up).
func (s *DecoySource) ListProfiles() (map[string]ClientProfileInfo, error) {
	ro, cancel := s.roUI()
	defer cancel()

	var rows []clientProfile
	if err := ro.Raw(
		"SELECT client, hour_hist_utc, first_seen, last_seen, updated_at FROM client_profile").
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	out := make(map[string]ClientProfileInfo, len(rows))
	for _, r := range rows {
		out[r.Client] = ClientProfileInfo{
			HourHistUTC: decodeHourHist(r.HourHistUTC),
			FirstSeen:   r.FirstSeen,
			LastSeen:    r.LastSeen,
			UpdatedAt:   r.UpdatedAt,
		}
	}

	return out, nil
}

// PurgeProfiles deletes all precomputed presence data (the profiling purge
// button). Local-only feature, so a purge is a plain table wipe.
func (s *DecoySource) PurgeProfiles() error {
	return s.db.Exec("DELETE FROM client_profile").Error
}

// sanitizeDisplayName bounds a user-supplied client name: control characters are
// stripped (it is rendered in the UI), the result is trimmed and capped at 63
// runes. Unicode letters/marks are kept so real names ("Alex's iPhone") survive.
func sanitizeDisplayName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}

		return r
	}, s)
	s = strings.TrimSpace(s)

	if r := []rune(s); len(r) > 63 {
		s = string(r[:63])
	}

	return s
}

// ClientName returns the manual display-name override for client, or "" when
// none is set. One primary-key lookup — safe to call per row. Mirrors ClientClass.
func (s *DecoySource) ClientName(client string) (string, error) {
	var name string

	ro, cancel := s.roUI()
	defer cancel()

	// Resolve to the stable device_key first, so a display name survives an IP change.
	err := ro.Raw("SELECT name FROM client_identity WHERE client = ? LIMIT 1", s.deviceKey(client)).Scan(&name).Error

	return name, err
}

// ClientNames returns every set display-name override as client_name→name, so
// the clients list can layer names in one query instead of one lookup per row.
func (s *DecoySource) ClientNames() (map[string]string, error) {
	ro, cancel := s.roUI()
	defer cancel()

	var rows []clientIdentity
	if err := ro.Raw("SELECT client, name FROM client_identity WHERE name <> ''").Scan(&rows).Error; err != nil {
		return nil, err
	}

	byKey := make(map[string]string, len(rows))
	for _, r := range rows {
		byKey[r.Client] = r.Name
	}

	return s.fanKeysToNames(byKey), nil
}

// fanKeysToNames turns a device_key→value map (how association rows are stored)
// into a client_name→value map (how the name-keyed ClientList/people rollups look
// values up). Every current client_name presenting a keyed device gets the value
// — so a device that changed IP is found under its NEW name, and a cohort of
// identical stacks all resolve (the accepted fp-cohort ceiling). Raw keys are
// also passed through so idle (no window traffic) mappings still surface.
func (s *DecoySource) fanKeysToNames(byKey map[string]string) map[string]string {
	out := make(map[string]string, len(byKey))
	for k, v := range byKey {
		out[k] = v // passthrough: idle mappings + fp-less/legacy name keys
	}

	for name, key := range s.dominantFPByNameCached() {
		if v := byKey[key]; v != "" {
			out[name] = v
		}
	}

	return out
}

// SetClientName sets (or clears, with an empty/blank name) the display-name
// override for client. The name is sanitized (control chars stripped, capped at
// 63 runes) since it is rendered in the UI. Mirrors SetClientClassOverride:
// upsert, creates the row if the client has no identity yet.
func (s *DecoySource) SetClientName(client, name string) error {
	if client == "" {
		return errors.New("client must not be empty")
	}

	name = sanitizeDisplayName(name)
	now := time.Now()
	key := s.deviceKey(client) // store under the stable device_key

	err := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "client"}},
		DoUpdates: clause.Assignments(map[string]any{"name": name, "updated_at": now}),
	}).Create(&clientIdentity{Client: key, Name: name, UpdatedAt: now}).Error
	if err == nil {
		s.kickDominantFP() // pick up a just-named new device (single-flighted)
	}

	return err
}

// ClientPerson returns the household-member mapping for client, or "" when none
// is set. One primary-key lookup. Mirrors ClientName. Phase 5 — the most
// sensitive layer; callers gate on the profiling opt-in before exposing it.
func (s *DecoySource) ClientPerson(client string) (string, error) {
	var person string

	ro, cancel := s.roUI()
	defer cancel()

	// Resolve to the stable device_key first (IP-independent person mapping).
	err := ro.Raw("SELECT person FROM client_person WHERE client = ? LIMIT 1", s.deviceKey(client)).Scan(&person).Error

	return person, err
}

// ClientPersons returns every set device→person mapping as client_name→person,
// so the people page can roll up per-person footprints in one query. Mirrors
// ClientNames.
func (s *DecoySource) ClientPersons() (map[string]string, error) {
	ro, cancel := s.roUI()
	defer cancel()

	var rows []clientPerson
	if err := ro.Raw("SELECT client, person FROM client_person WHERE person <> ''").Scan(&rows).Error; err != nil {
		return nil, err
	}

	byKey := make(map[string]string, len(rows))
	for _, r := range rows {
		byKey[r.Client] = r.Person
	}

	return s.fanKeysToNames(byKey), nil
}

// SetClientPerson sets (or clears, with a blank person) the household-member
// mapping for client. The person label is sanitized (rendered in the UI) and
// capped like a display name. Mirrors SetClientName: upsert, creates the row if
// the client has no mapping yet. A client_name rename detaches the mapping from
// the device's old-name history (blueprint R6/C3) — the UI warns before a rename.
func (s *DecoySource) SetClientPerson(client, person string) error {
	if client == "" {
		return errors.New("client must not be empty")
	}

	person = sanitizeDisplayName(person)
	now := time.Now()
	key := s.deviceKey(client) // store under the stable device_key

	err := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "client"}},
		DoUpdates: clause.Assignments(map[string]any{"person": person, "updated_at": now}),
	}).Create(&clientPerson{Client: key, Person: person, UpdatedAt: now}).Error
	if err == nil {
		s.kickDominantFP() // pick up a just-mapped new device (single-flighted)
	}

	return err
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
