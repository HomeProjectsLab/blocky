package querylog

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/0xERR0R/blocky/model"

	gormlogger "gorm.io/gorm/logger"
)

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

		for _, detail := range explainPlan(t, r, sql) {
			up := strings.ToUpper(strings.TrimSpace(detail))
			if strings.HasPrefix(up, "SCAN") && namesBigTable(up) && !strings.Contains(up, "USING") {
				t.Errorf("full table scan of a large table:\n  plan: %s\n  sql:  %s", detail, sql)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no captured statement touched a large table — seeding or capture is wrong")
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
// interpolated, so no bind params are needed).
func explainPlan(t *testing.T, r *Reader, sql string) []string {
	t.Helper()

	var rows []struct {
		Detail string `gorm:"column:detail"`
	}

	if err := r.db.Raw("EXPLAIN QUERY PLAN " + sql).Scan(&rows).Error; err != nil {
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
