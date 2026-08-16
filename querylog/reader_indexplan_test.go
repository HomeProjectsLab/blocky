package querylog

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// clientScopedRe matches a query that filters on a specific client (client_name =
// '<literal>' / "<literal>"), as opposed to grouping by client_name or joining on
// it. Such a query MUST ride idx_client_name_request_ts, never idx_decoy_request_ts.
var clientScopedRe = regexp.MustCompile(`client_name\s*=\s*['"]`)

// assertClientScopedRidesRightIndex is the #10 guard the bare-scan check above
// misses: without an INDEXED BY hint the planner picks idx_decoy_request_ts
// (decoy=0 prefix) for a client-scoped query and scans the whole decoy=0 partition
// filtering client_name in memory — a SEARCH on the WRONG index, which passes the
// "SCAN without USING" gate green (this is how finding #1 regressed uncaught).
func assertClientScopedRidesRightIndex(t *testing.T, db *gorm.DB, sql string) {
	t.Helper()

	if !clientScopedRe.MatchString(strings.ToLower(sql)) {
		return
	}

	for _, detail := range explainPlan(t, db, sql) {
		if strings.Contains(detail, "idx_decoy_request_ts") {
			t.Errorf("client-scoped query rides the WRONG index idx_decoy_request_ts "+
				"(pin idx_client_name_request_ts with INDEXED BY):\n  plan: %s\n  sql:  %s", detail, sql)
		}
	}
}

// captureLogger records the fully-interpolated SQL of every statement the
// reader executes, so the plan test can re-run each one under EXPLAIN QUERY
// PLAN. fc() returns the SQL with vars already substituted by the dialector,
// which EXPLAIN QUERY PLAN can consume verbatim.
type captureLogger struct {
	on  bool
	sql []string
}

func (c *captureLogger) LogMode(gormlogger.LogLevel) gormlogger.Interface { return c }
func (c *captureLogger) Info(context.Context, string, ...interface{})     {}
func (c *captureLogger) Warn(context.Context, string, ...interface{})     {}
func (c *captureLogger) Error(context.Context, string, ...interface{})    {}

func (c *captureLogger) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	if !c.on {
		return
	}

	sql, _ := fc()
	c.sql = append(c.sql, sql)
}

// bigTables are the log-scale tables whose reads must never full-scan. Any
// EXPLAIN QUERY PLAN "SCAN <table>" line for one of these must carry "USING
// ... INDEX" (a covering-index scan is fine; a bare table scan is not).
var bigTables = []string{"log_entries", "agg_hourly", "agg_domains_hourly"}

// TestUIQueriesAreIndexBacked seeds a realistic sqlite db, exercises every
// reader method that backs an /api/ui endpoint, captures the SQL each one runs,
// and asserts EXPLAIN QUERY PLAN never full-scans a large table. A future query
// rewrite that reintroduces a scan (e.g. dropping a time bound, or filtering on
// an unindexed column) fails here instead of in production.
func TestUIQueriesAreIndexBacked(t *testing.T) {
	w, r := writerReaderOnTemp(t)
	seedForPlan(t, w)
	waitForETLDPIndex(t, w) // etldp index is now built async (buildDeferredIndexes)

	cl := &captureLogger{on: true}
	r.db.Logger = cl

	from := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	to := from.Add(72 * time.Hour)

	// Exercise every UI-facing reader method. We ignore returned data — only the
	// SQL each one emits matters. Reset the TotalQueries cache so its COUNT(*)
	// actually runs and gets captured.
	r.totalAt = time.Time{}

	mustRun := func(name string, err error) {
		t.Helper()

		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	if _, err := r.Overview(from, to); err != nil {
		mustRun("Overview", err)
	}

	if _, err := r.Buckets(from, to, 3600); err != nil {
		mustRun("Buckets", err)
	}

	for _, col := range []string{"domain", "blocked", "client", "transport", "fphash"} {
		if _, err := r.Top(from, to, col, 10); err != nil {
			mustRun("Top:"+col, err)
		}
	}

	if _, err := r.LatencyPercentiles(from, to); err != nil {
		mustRun("LatencyPercentiles", err)
	}

	// Search: plain, plus the domain-LIKE and client-filter variants.
	for _, f := range []SearchFilter{
		{From: from, To: to},
		{From: from, To: to, Domain: "example.com"},
		{From: from, To: to, Domain: "ads.*"},
		{From: from, To: to, Client: "host-1"},
	} {
		if _, _, err := r.Search(f); err != nil {
			mustRun("Search", err)
		}
	}

	if _, err := r.TotalQueries(); err != nil {
		mustRun("TotalQueries", err)
	}

	// ClientList/ClientDetail run the multi-facet enrich SELECT (enrichClients/
	// enrichClient in client_enrich.go). The added os MAX(CASE) + vendor/model/app
	// GROUP_CONCAT(DISTINCT CASE) columns keep the same GROUP BY client_name scan
	// and request_ts bound (no window fn), so the captured SQL below must still
	// come back index-backed — this is the blueprint-R1 facet-enrich assertion.
	if _, err := r.ClientList(from, to); err != nil {
		mustRun("ClientList", err)
	}

	if _, err := r.ClientDetail("host-1", from, to); err != nil {
		mustRun("ClientDetail", err)
	}

	// Phase-4 categories: per-client (question_name on idx_client_name_request_ts)
	// and the global timeline (etldp subset on agg_domains_hourly) must both stay
	// index-backed — same scan shapes as TopDomains / Top("domain").
	if _, err := r.ClientCategories("host-1", from, to); err != nil {
		mustRun("ClientCategories", err)
	}

	if _, err := r.CategoryTotals(from, to); err != nil {
		mustRun("CategoryTotals", err)
	}

	if _, err := r.DecoyOverview(from, to); err != nil {
		mustRun("DecoyOverview", err)
	}

	if _, err := r.DecoySourceMix(from, to); err != nil {
		mustRun("DecoySourceMix", err)
	}

	if _, err := r.DecoyTopDomains(from, to, 10); err != nil {
		mustRun("DecoyTopDomains", err)
	}

	if _, err := r.DecoyBuckets(from, to, 3600); err != nil {
		mustRun("DecoyBuckets", err)
	}

	cl.on = false // stop capturing before we run the EXPLAINs themselves

	if len(cl.sql) == 0 {
		t.Fatal("captured no SQL — logger wiring is broken")
	}

	checked := 0

	for _, sql := range cl.sql {
		low := strings.ToLower(sql)
		if !touchesBigTable(low) {
			continue
		}

		checked++

		for _, detail := range explainPlan(t, r.db, sql) {
			up := strings.ToUpper(strings.TrimSpace(detail))
			if strings.HasPrefix(up, "SCAN") && namesBigTable(up) && !strings.Contains(up, "USING") {
				t.Errorf("full table scan of a large table:\n  plan: %s\n  sql:  %s", detail, sql)
			}
		}

		assertClientScopedRidesRightIndex(t, r.db, sql)
	}

	if checked == 0 {
		t.Fatal("no captured statement touched a large table — seeding or capture is wrong")
	}
}

// TestDecoySamplersAreIndexBacked is the decoy-side sibling of the UI gate: it
// seeds the same realistic db, opens a DecoySource on it, attaches the capture
// logger to its read-only sampling pool, exercises every Sample*/read on the
// emit hot path, and asserts EXPLAIN QUERY PLAN never bare-scans log_entries.
// An ORDER BY RANDOM() (or otherwise unindexed) regression fails here, not in
// production where it silently caps the decoy emit rate.
func TestDecoySamplersAreIndexBacked(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "q.db")

	w, err := NewDatabaseWriter(context.Background(), config.QueryLogTypeSqlite, dbPath, 7, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if sqlDB, err := w.db.DB(); err == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}

	seedForPlan(t, w)
	waitForETLDPIndex(t, w) // etldp index is now built async (buildDeferredIndexes)

	src, err := NewDecoySource(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = src.Close() })

	// Populate client_class + the materialized session models so SampleClientOfClass
	// and the Markov samplers exercise their real (non-empty) read paths.
	if err := src.RefreshClientClasses(); err != nil {
		t.Fatal(err)
	}

	cl := &captureLogger{on: true}
	src.ro.Logger = cl

	// Force a fresh MIN/MAX rowid bounds probe so it, too, is captured + checked.
	src.mu.Lock()
	src.leRowidAt = time.Time{}
	src.mu.Unlock()

	mustRun := func(name string, err error) {
		t.Helper()

		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	if _, err := src.SampleList(); err != nil {
		mustRun("SampleList", err)
	}

	if _, err := src.SampleRealFingerprint(); err != nil {
		mustRun("SampleRealFingerprint", err)
	}

	// www.example.com. has real history in the seed, so the etldp lookup path runs.
	if _, err := src.SampleFingerprintForName("www.example.com."); err != nil {
		mustRun("SampleFingerprintForName", err)
	}

	if _, err := src.SampleClient(); err != nil {
		mustRun("SampleClient", err)
	}

	if _, err := src.SampleRecentReal(3); err != nil {
		mustRun("SampleRecentReal", err)
	}

	if _, err := src.SampleCohort(); err != nil {
		mustRun("SampleCohort", err)
	}

	if _, err := src.NextInSession("example.com"); err != nil {
		mustRun("NextInSession", err)
	}

	if _, err := src.SessionSeed(); err != nil {
		mustRun("SessionSeed", err)
	}

	// RevisitInterval returns (dur, ok), no error channel — run it for its SQL.
	src.RevisitInterval("www.example.com.")

	// Every effective class, so at least one has a matching client and the
	// per-client log_entries read (2nd query) actually runs and is captured.
	for _, class := range []string{ClassIoT, ClassWorkstation, ClassServer, ClassUnknown} {
		if _, err := src.SampleClientOfClass(class); err != nil {
			mustRun("SampleClientOfClass:"+class, err)
		}
	}

	cl.on = false // stop capturing before we run the EXPLAINs themselves

	if len(cl.sql) == 0 {
		t.Fatal("captured no SQL — logger wiring is broken")
	}

	checked := 0

	for _, sql := range cl.sql {
		low := strings.ToLower(sql)
		if !touchesBigTable(low) {
			continue
		}

		checked++

		for _, detail := range explainPlan(t, src.ro, sql) {
			up := strings.ToUpper(strings.TrimSpace(detail))
			if strings.HasPrefix(up, "SCAN") && namesBigTable(up) && !strings.Contains(up, "USING") {
				t.Errorf("decoy sampler full-scans a large table:\n  plan: %s\n  sql:  %s", detail, sql)
			}
		}

		// Covers SampleClientOfClass (decoy_source.go): both its per-client reads
		// must pin idx_client_name_request_ts, not ride idx_decoy_request_ts.
		assertClientScopedRidesRightIndex(t, src.ro, sql)
	}

	if checked == 0 {
		t.Fatal("no captured decoy statement touched a large table — seeding or capture is wrong")
	}
}

// TestDeferredVacuumGatedButIndexAlwaysBuilt is the #9 (H1) regression guard. The
// etldp index build used to be bundled behind the VACUUM size ceiling, so on any
// box over 256MiB the index was never built and the decoy emit hot path silently
// full-scanned the decoy=0 partition. Force the over-ceiling branch with a tiny
// ceiling and assert the index is STILL built while the VACUUM is skipped. The
// existing waitForETLDPIndex assertion only runs on a small temp DB where the guard
// never trips, so it cannot catch this.
func TestDeferredVacuumGatedButIndexAlwaysBuilt(t *testing.T) {
	orig := heavyMaintenanceCeilingBytes
	heavyMaintenanceCeilingBytes = 1 // any real DB is "over" 1 byte → over-ceiling branch
	t.Cleanup(func() { heavyMaintenanceCeilingBytes = orig })

	// Construction spawns buildDeferredIndexes, which now reads the tiny ceiling.
	w, _ := writerReaderOnTemp(t)

	// #9: the etldp index must be built even over the ceiling (fails if re-bundled).
	waitForETLDPIndex(t, w)

	// Prove the decouple: the size guard skipped the VACUUM, so auto_vacuum was never
	// switched to INCREMENTAL(2) — it stays at the fresh-DB default NONE(0).
	var mode int
	if err := w.db.Raw("PRAGMA auto_vacuum").Scan(&mode).Error; err != nil {
		t.Fatal(err)
	}

	if mode == 2 {
		t.Fatal("VACUUM ran under the over-ceiling branch: the size guard must gate ONLY the VACUUM, not the index")
	}
}

// --- heuristics index-plan harness (blueprint §5.1) ---------------------------

// heuristicsTableNames are the durable heuristics/persona tables. Any live fold/
// scorer statement touching one of these must be purge-immune (never joins
// log_entries) — the structural guarantee that intelligence survives a purge.
// ("device_class" as a substring also matches _signal / _domainset, which is what
// we want for the touches-heuristics test.)
var heuristicsTableNames = []string{
	"device_identity", "fp_binding", "device_facet", "device_presence",
	"service_usage", "category_usage", "device_class", "device_class_signal",
	"device_class_domainset", "persona_link",
}

// hquery is one heuristics read shape plus the index it MUST plan onto. wantIdx=""
// means "any index / PK is fine, just not a bare table scan" — enough to fail
// loudly if a PK or index is ever dropped (the point read degrades to SCAN).
type hquery struct {
	name    string
	sql     string
	wantIdx string // "" => must be index-backed on *some* index; else this exact name
}

// heuristicsQueries is the anti-rot registry of every heuristics/persona read the
// schema's indexes exist to serve (blueprint §2.9). TestHeuristicsQueriesAreIndexBacked
// EXPLAINs each and asserts it plans onto an index — even on the small tables, so a
// dropped PK/index fails here instead of silently degrading to a scan in prod.
// ponytail: static list, not an AST walker — add the parser only past ~50 queries.
var heuristicsQueries = []hquery{
	// device_identity — PK point read + recency ordering
	{"identity_point", "SELECT * FROM device_identity WHERE fp_hash = 'fp1'", ""},
	{"identity_recency", "SELECT fp_hash FROM device_identity ORDER BY last_seen DESC LIMIT 20", "idx_device_identity_last_seen"},
	{"identity_new48h", "SELECT fp_hash FROM device_identity WHERE last_seen > '2026-01-01T00:00:00Z' ORDER BY last_seen DESC", "idx_device_identity_last_seen"},
	// fp_binding — FpCount rides the (client_name,fp_hash) PK prefix, NOT idx_fp_binding_fp
	{"fpcount", "SELECT COUNT(*) FROM fp_binding WHERE client_name = 'host-1'", ""},
	{"fp_group_by_identity", "SELECT client_name FROM fp_binding WHERE fp_hash = 'fp1'", "idx_fp_binding_fp"},
	{"distinct_clients_rollup", "SELECT COUNT(*) FROM fp_binding WHERE fp_binding.fp_hash = 'fp1'", "idx_fp_binding_fp"},
	// device_facet — per-device recognition, OS max-conf, fleet rollup
	{"facet_device", "SELECT facet, label, conf FROM device_facet WHERE fp_hash = 'fp1'", ""},
	{"facet_os_pick", "SELECT label FROM device_facet WHERE fp_hash = 'fp1' AND facet = 'os' ORDER BY conf DESC LIMIT 1", ""},
	{"facet_fleet", "SELECT COUNT(*) FROM device_facet WHERE facet = 'vendor' AND label = 'Apple'", "idx_device_facet_label"},
	// device_presence — per-device histogram + the planning heatmap
	{"presence_device", "SELECT dow, hour, cnt FROM device_presence WHERE fp_hash = 'fp1'", ""},
	{"presence_heatmap", "SELECT fp_hash, cnt FROM device_presence WHERE dow = 2 AND hour = 20 ORDER BY cnt DESC", "idx_device_presence_dowhour"},
	// service_usage — per-device list + fleet top-services
	{"service_device", "SELECT service, hits FROM service_usage WHERE fp_hash = 'fp1'", ""},
	{"service_fleet", "SELECT service, SUM(hits) AS h FROM service_usage GROUP BY service ORDER BY h DESC", "idx_service_usage_service"},
	// category_usage — per-device magnitude + fleet CategoryTotals (replaces agg_domains_hourly)
	{"category_device", "SELECT category, hits FROM category_usage WHERE fp_hash = 'fp1'", ""},
	{"category_fleet", "SELECT category, SUM(hits) AS h FROM category_usage GROUP BY category", "idx_category_usage_category"},
	// class — served class point read + the scorer's cursor read + IoT-domain count
	{"class_point", "SELECT class, override FROM device_class WHERE fp_hash = 'fp1'", ""},
	{"class_signal_cursor", "SELECT fp_hash, last_ts, domains FROM device_class_signal WHERE fp_hash IN ('fp1','fp2')", ""},
	{"domainset_count", "SELECT fp_hash, COUNT(*) FROM device_class_domainset WHERE fp_hash IN ('fp1') GROUP BY fp_hash", ""},
	// persona — "who is this device" + "all devices for person=Alice"
	{"persona_device", "SELECT person FROM persona_link WHERE fp_hash = 'fp1'", ""},
	{"persona_person", "SELECT fp_hash FROM persona_link WHERE person = 'Alice'", "idx_persona_link_person"},
}

// planUsesIndex reports whether the plan has a step that reads via an index/PK
// (SEARCH/SCAN ... USING INDEX | USING PRIMARY KEY | USING COVERING INDEX).
func planUsesIndex(plan []string) bool {
	for _, d := range plan {
		if strings.Contains(strings.ToUpper(d), "USING") {
			return true
		}
	}

	return false
}

func planMentions(plan []string, idx string) bool {
	for _, d := range plan {
		if strings.Contains(d, idx) {
			return true
		}
	}

	return false
}

// TestHeuristicsQueriesAreIndexBacked is the blueprint §5.1 gate for the durable
// heuristics layer. It migrates the heuristics tables and EXPLAINs every read in
// the heuristicsQueries registry, asserting each plans onto an index (assertion 2,
// applied even to the small tables), never a bare table scan (assertion 1). It
// then pins the one wrong-index temptation with a negative assertion: FpCount's
// client_name filter must ride the (client_name,fp_hash) PK prefix, NEVER
// idx_fp_binding_fp (which is fp_hash-first and useless for that predicate).
func TestHeuristicsQueriesAreIndexBacked(t *testing.T) {
	db := openHeuristicsDB(t)

	for _, q := range heuristicsQueries {
		plan := explainPlan(t, db, q.sql)

		// Assertion 1: no bare scan of a heuristics table (all are small, so ANY
		// SCAN without USING means a dropped index/PK).
		for _, detail := range plan {
			up := strings.ToUpper(strings.TrimSpace(detail))
			if strings.HasPrefix(up, "SCAN") && !strings.Contains(up, "USING") {
				t.Errorf("%s: bare table scan (dropped PK/index?):\n  plan: %v\n  sql:  %s", q.name, plan, q.sql)
			}
		}

		// Assertion 2: index-backed, on the specific index when one is named.
		if !planUsesIndex(plan) {
			t.Errorf("%s: not index-backed — a dropped PK/index degraded it to a scan:\n  plan: %v\n  sql:  %s",
				q.name, plan, q.sql)
		}

		if q.wantIdx != "" && !planMentions(plan, q.wantIdx) {
			t.Errorf("%s: expected to ride %q but did not:\n  plan: %v\n  sql:  %s",
				q.name, q.wantIdx, plan, q.sql)
		}
	}

	// Negative wrong-index guard: FpCount (client_name filter) must use the PK
	// prefix, not the fp_hash-keyed idx_fp_binding_fp. A planner slip here would
	// scan the whole fp_hash index filtering client_name in memory (the §5.1
	// wrong-index hazard, sibling of the idx_decoy_request_ts trap).
	fpCount := "SELECT COUNT(*) FROM fp_binding WHERE client_name = 'host-1'"
	if plan := explainPlan(t, db, fpCount); planMentions(plan, "idx_fp_binding_fp") {
		t.Errorf("FpCount rides the WRONG index idx_fp_binding_fp (must use the "+
			"(client_name,fp_hash) PK prefix):\n  plan: %v\n  sql:  %s", plan, fpCount)
	}
}

// TestHeuristicsFoldTouchesNoLogEntries is blueprint §5.1 assertion 3 — the
// structural purge-immunity guarantee. It captures every statement the live write
// path (the fold) and the class scorer actually run, and asserts NONE references
// log_entries and NONE bare-scans a big table. If a future change re-derived any
// heuristic from a log_entries scan, a purge would wipe it — and this fails.
func TestHeuristicsFoldTouchesNoLogEntries(t *testing.T) {
	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	stream := beaconEntries("host-1", "fp1", []string{"telemetry.tuya.com", "apple.com", "time.nist.gov"}, 30, base, 5*time.Minute)

	// Fold path: capture upsertHeuristics' cursor SELECT, domainset SELECT + upserts.
	foldDB := openHeuristicsDB(t)
	foldCap := &captureLogger{on: true}
	foldDB.Logger = foldCap

	if err := upsertHeuristics(foldDB, stream); err != nil {
		t.Fatal(err)
	}

	if err := pruneServiceUsage(foldDB); err != nil {
		t.Fatal(err)
	}

	foldCap.on = false

	// Scorer path: capture scoreDeviceClasses (Find + device_class/client_class
	// upserts + the distinct_clients rollup + checkpoint) on a real DecoySource.
	dbPath := filepath.Join(t.TempDir(), "q.db")

	w, err := NewDatabaseWriter(context.Background(), config.QueryLogTypeSqlite, dbPath, 7, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 30; i++ {
		w.Write(&LogEntry{
			Start:        base.Add(time.Duration(i) * 5 * time.Minute),
			ClientNames:  []string{"host-1"},
			QuestionName: []string{"telemetry.tuya.com", "time.nist.gov"}[i%2],
			QuestionType: "A", ResponseType: "RESOLVED",
		})
	}

	if err := w.doDBWrite(); err != nil {
		t.Fatal(err)
	}

	if sqlDB, e := w.db.DB(); e == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}

	src, err := NewDecoySource(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = src.Close() })

	scorerCap := &captureLogger{on: true}
	src.db.Logger = scorerCap

	if err := src.scoreDeviceClasses(); err != nil {
		t.Fatal(err)
	}

	scorerCap.on = false

	captured := append(append([]string{}, foldCap.sql...), scorerCap.sql...)
	if len(captured) == 0 {
		t.Fatal("captured no SQL — logger wiring is broken")
	}

	sawHeuristicsStmt := false

	for _, sql := range captured {
		low := strings.ToLower(sql)

		touchesHeuristics := false

		for _, tbl := range heuristicsTableNames {
			if strings.Contains(low, tbl) {
				touchesHeuristics = true

				break
			}
		}

		if !touchesHeuristics {
			continue // raw insert / aggregate / pragma — not a heuristics statement
		}

		sawHeuristicsStmt = true

		// Assertion 3: a heuristics statement must never join / read log_entries.
		if strings.Contains(low, "log_entries") {
			t.Errorf("heuristics statement references log_entries — a purge would wipe it:\n  sql: %s", sql)
		}

		// Assertion 1 on the live path: no bare scan of a BIG table (the small
		// heuristics tables are allowed to scan — scorer Find + prune do, by design).
		if !touchesBigTable(low) {
			continue
		}

		for _, detail := range explainPlan(t, src.db, sql) {
			up := strings.ToUpper(strings.TrimSpace(detail))
			if strings.HasPrefix(up, "SCAN") && namesBigTable(up) && !strings.Contains(up, "USING") {
				t.Errorf("heuristics statement full-scans a big table:\n  plan: %s\n  sql: %s", detail, sql)
			}
		}
	}

	if !sawHeuristicsStmt {
		t.Fatal("captured no heuristics statement — fold/scorer wiring or capture is wrong")
	}
}

func touchesBigTable(lowerSQL string) bool {
	for _, tbl := range bigTables {
		if strings.Contains(lowerSQL, tbl) {
			return true
		}
	}

	return false
}

func namesBigTable(upperDetail string) bool {
	for _, tbl := range bigTables {
		if strings.Contains(upperDetail, strings.ToUpper(tbl)) {
			return true
		}
	}

	return false
}

// explainPlan returns the detail column of EXPLAIN QUERY PLAN for sql (already
// interpolated, so no bind params are needed). db is any handle onto the same
// database — the UI reader's or the decoy source's read-only pool.
func explainPlan(t *testing.T, db *gorm.DB, sql string) []string {
	t.Helper()

	var rows []struct {
		Detail string `gorm:"column:detail"`
	}

	if err := db.Raw("EXPLAIN QUERY PLAN " + sql).Scan(&rows).Error; err != nil {
		t.Fatalf("EXPLAIN failed for %q: %v", sql, err)
	}

	out := make([]string, len(rows))
	for i := range rows {
		out[i] = rows[i].Detail
	}

	return out
}

// seedForPlan writes a realistic spread of rows (3 days, several clients and
// domains, a decoy fraction) and flushes so both log_entries and the hourly
// aggregate tables are populated before the planner sees them.
func seedForPlan(t *testing.T, w *DatabaseWriter) {
	t.Helper()

	base := time.Date(2026, 8, 13, 0, 30, 0, 0, time.UTC)
	domains := []string{"www.example.com.", "cdn.example.com.", "ads.tracker.net.", "api.service.io."}
	clients := []string{"host-1", "host-2", "host-3"}
	rtypes := []string{"RESOLVED", "RESOLVED", "CACHED", "BLOCKED"}
	transports := []model.Transport{model.TransportDo53UDP, model.TransportDoT, model.TransportDoH}

	for i := 0; i < 3000; i++ {
		decoy := i%5 == 0

		e := &LogEntry{
			Start:        base.Add(time.Duration(i) * 80 * time.Second),
			ClientIP:     fmt.Sprintf("10.0.0.%d", i%3+1),
			ClientNames:  []string{clients[i%len(clients)]},
			QuestionName: domains[i%len(domains)],
			QuestionType: "A",
			ResponseType: rtypes[i%len(rtypes)],
			ResponseCode: "NOERROR",
			DurationMs:   int64(i%150 + 1),
			Decoy:        decoy,
			Fingerprint:  model.Fingerprint{Transport: transports[i%len(transports)]},
		}
		if decoy {
			e.DecoySource = []string{"replay", "corpus", "cohort"}[i%3]
		}

		w.Write(e)
	}

	if err := w.doDBWrite(); err != nil {
		t.Fatal(err)
	}
}

// waitForETLDPIndex blocks until the background builder (buildDeferredIndexes) has
// landed idx_log_entries_etldp_ts, so the index-plan gates don't race it. On a
// small temp db the build is milliseconds; also asserts the deferred build ran at
// all (a regression that never creates the index trips the deadline here).
func waitForETLDPIndex(t *testing.T, w *DatabaseWriter) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		if err := w.db.Raw(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_log_entries_etldp_ts'").
			Scan(&n).Error; err == nil && n == 1 {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("idx_log_entries_etldp_ts was not built by the background builder within 10s")
}
