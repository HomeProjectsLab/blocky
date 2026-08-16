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
