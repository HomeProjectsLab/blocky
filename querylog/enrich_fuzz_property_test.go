//go:build !mips && !mipsle && !mips64 && !mips64le && !loong64 && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package querylog

import (
	"context"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"

	"github.com/glebarez/sqlite"
)

// Standard `go test` fuzz + property coverage for the client-enrichment and
// prune paths. Lives alongside the Ginkgo suite (different funcs, same package):
// `go test` runs both. Fuzz corpora double as seed-regression tests once the
// -fuzz flag is dropped.

// newRawWriter builds an in-memory sqlite DatabaseWriter for direct-row tests,
// bypassing the exported NewDatabaseWriter so no disk-guardian goroutine starts.
func newRawWriter(tb testing.TB) *DatabaseWriter {
	tb.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	tb.Cleanup(cancel)

	w, err := newDatabaseWriter(ctx, sqlite.Open("file::memory:"), 7, time.Minute, config.QueryLogTypeSqlite)
	if err != nil {
		tb.Fatalf("newDatabaseWriter: %v", err)
	}

	sqlDB, err := w.db.DB()
	if err != nil {
		tb.Fatalf("db handle: %v", err)
	}

	tb.Cleanup(func() { _ = sqlDB.Close() })

	return w
}

// --- FuzzToEnrich: the pure IPs-parse + threshold logic ---------------------

func FuzzToEnrich(f *testing.F) {
	f.Add("10.0.0.1,10.0.0.2", 3)
	f.Add("", 8)
	f.Add("  ,  ", 0)
	f.Add("a,,b, c ", 100)
	f.Add("10.0.0.1,10.0.0.1", -1)

	f.Fuzz(func(t *testing.T, ipsCSV string, fpCount int) {
		e := clientEnrichRow{IPs: ipsCSV, FpCount: fpCount}.toEnrich()

		if e.FpCount != fpCount {
			t.Fatalf("FpCount mangled: got %d want %d", e.FpCount, fpCount)
		}

		if want := fpCount >= natAggregateFpThreshold; e.NatAggregate != want {
			t.Fatalf("NatAggregate=%v for fpCount=%d (threshold %d)", e.NatAggregate, fpCount, natAggregateFpThreshold)
		}

		fields := strings.Count(ipsCSV, ",") + 1
		if len(e.IPs) > fields {
			t.Fatalf("produced more IPs (%d) than comma fields (%d)", len(e.IPs), fields)
		}

		for _, ip := range e.IPs {
			if strings.TrimSpace(ip) == "" {
				t.Fatalf("empty/blank IP survived parse from %q", ipsCSV)
			}
		}
	})
}

// --- FuzzEnrichClients: arbitrary strings through the real SQL path ----------

func FuzzEnrichClients(f *testing.F) {
	f.Add("laptop", "10.0.0.5", "www.example.com.", "fp-a")
	f.Add("o'brien", "10.0.0.5", "a'b--c", "fp") // quote / SQL-ish
	f.Add("r", "1,2,3", "x%y_z", "")             // comma in IP, LIKE wildcards
	f.Add("", "", "", "")

	w := newRawWriter(f)
	reader := &Reader{db: w.db}
	from := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	base := from.Add(6 * time.Hour)
	to := from.Add(23 * time.Hour)

	f.Fuzz(func(t *testing.T, name, ip, question, fpHash string) {
		if err := w.db.Exec("DELETE FROM log_entries").Error; err != nil {
			t.Fatalf("reset: %v", err)
		}

		// one real row plus a decoy carrying the same name but a distinct fp:
		// the decoy must never influence the enrichment result.
		mk := func(fp string, decoy bool, off time.Duration) {
			if err := w.db.Create(&logEntry{
				RequestTS: base.Add(off), ClientName: name, ClientIP: ip,
				QuestionName: question, FpHash: fp, Decoy: decoy,
			}).Error; err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
		mk(fpHash, false, 0)
		mk(fpHash+"-decoy", true, time.Minute)

		out, err := reader.enrichClients(from, to)
		if err != nil {
			t.Fatalf("enrichClients errored on name=%q ip=%q q=%q fp=%q: %v", name, ip, question, fpHash, err)
		}

		e, ok := out[name]
		if !ok {
			t.Fatalf("real row for client %q missing from result", name)
		}

		// decoy excluded: only the single real fp (if non-empty) counts.
		wantFp := 0
		if fpHash != "" {
			wantFp = 1
		}

		if e.FpCount != wantFp {
			t.Fatalf("FpCount=%d want %d (decoy leaked?) fp=%q", e.FpCount, wantFp, fpHash)
		}

		if e.NatAggregate != (e.FpCount >= natAggregateFpThreshold) {
			t.Fatalf("NatAggregate=%v inconsistent with FpCount=%d", e.NatAggregate, e.FpCount)
		}
	})
}

// --- Integration: real Write+flush, then enrich vs independent SQL recount ---

func TestEnrichAfterFlushIntegration(t *testing.T) {
	w := newRawWriter(t)
	reader := &Reader{db: w.db}
	rng := rand.New(rand.NewSource(3))

	from := time.Now().Add(-24 * time.Hour)
	base := time.Now().Add(-6 * time.Hour)
	to := time.Now()

	names := []string{"alpha", "bravo"}
	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}

	for i := 0; i < 120; i++ {
		name := names[rng.Intn(len(names))]
		decoy := rng.Intn(3) == 0 // ~1/3 decoys
		w.Write(&LogEntry{
			Start:        base.Add(time.Duration(i) * time.Minute),
			ClientNames:  []string{name},
			ClientIP:     ips[rng.Intn(len(ips))],
			QuestionName: "host.example.com.",
			ResponseType: "RESOLVED",
			Decoy:        decoy,
			// vary a hashed fingerprint field so distinct real stacks appear
			Fingerprint: model.Fingerprint{Transport: model.TransportDoT, TLSCipher: uint16(rng.Intn(12))},
		})
	}

	if err := w.doDBWrite(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	out, err := reader.enrichClients(from, to)
	if err != nil {
		t.Fatalf("enrichClients: %v", err)
	}

	// Independent recount with a different query shape (plain COUNT/GROUP, no
	// GROUP_CONCAT or CASE) than enrichClients uses.
	type recount struct {
		Name    string
		FpCount int
		IPCount int
	}
	var rows []recount
	if err := w.db.Raw(`SELECT client_name AS name,
		COUNT(DISTINCT NULLIF(fp_hash,'')) AS fp_count,
		COUNT(DISTINCT client_ip) AS ip_count
		FROM log_entries WHERE decoy = 0 GROUP BY client_name`).Scan(&rows).Error; err != nil {
		t.Fatalf("recount: %v", err)
	}

	if len(rows) != len(out) {
		t.Fatalf("recount has %d clients, enrich has %d", len(rows), len(out))
	}

	for _, rc := range rows {
		e, ok := out[rc.Name]
		if !ok {
			t.Fatalf("client %q in recount missing from enrich", rc.Name)
		}
		if e.FpCount != rc.FpCount {
			t.Fatalf("client %q FpCount enrich=%d recount=%d", rc.Name, e.FpCount, rc.FpCount)
		}
		if len(e.IPs) != rc.IPCount {
			t.Fatalf("client %q distinct IPs enrich=%d recount=%d", rc.Name, len(e.IPs), rc.IPCount)
		}
		if e.NatAggregate != (rc.FpCount >= natAggregateFpThreshold) {
			t.Fatalf("client %q NatAggregate=%v fpCount=%d", rc.Name, e.NatAggregate, rc.FpCount)
		}
	}
}

// --- Property: enrichClients over random rows matches an in-Go recount -------

func TestEnrichClientsProperty(t *testing.T) {
	w := newRawWriter(t)
	reader := &Reader{db: w.db}
	rng := rand.New(rand.NewSource(1))

	from := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	base := from.Add(6 * time.Hour)
	to := from.Add(23 * time.Hour)

	names := []string{"alpha", "bravo", "charlie"}
	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}

	for trial := 0; trial < 300; trial++ {
		if err := w.db.Exec("DELETE FROM log_entries").Error; err != nil {
			t.Fatalf("reset: %v", err)
		}

		wantIPs := map[string]map[string]bool{} // real rows only
		wantFps := map[string]map[string]bool{} // real, non-empty fp only

		n := rng.Intn(40)
		for i := 0; i < n; i++ {
			name := names[rng.Intn(len(names))]
			ip := ips[rng.Intn(len(ips))]
			decoy := rng.Intn(2) == 0

			fp := ""
			if rng.Intn(3) != 0 { // sometimes empty
				fp = "fp-" + strconv.Itoa(rng.Intn(20))
			}

			if err := w.db.Create(&logEntry{
				RequestTS:  base.Add(time.Duration(i) * time.Minute),
				ClientName: name, ClientIP: ip, FpHash: fp, Decoy: decoy,
			}).Error; err != nil {
				t.Fatalf("insert: %v", err)
			}

			if decoy {
				continue
			}

			if wantIPs[name] == nil {
				wantIPs[name] = map[string]bool{}
				wantFps[name] = map[string]bool{}
			}
			wantIPs[name][ip] = true
			if fp != "" {
				wantFps[name][fp] = true
			}
		}

		out, err := reader.enrichClients(from, to)
		if err != nil {
			t.Fatalf("enrichClients: %v", err)
		}

		if len(out) != len(wantIPs) {
			t.Fatalf("trial %d: %d clients, want %d", trial, len(out), len(wantIPs))
		}

		for name, e := range out {
			if wantIPs[name] == nil {
				t.Fatalf("client %q returned but has no real rows (decoy counted)", name)
			}

			if e.FpCount != len(wantFps[name]) {
				t.Fatalf("client %q FpCount=%d want %d", name, e.FpCount, len(wantFps[name]))
			}

			if e.NatAggregate != (len(wantFps[name]) >= natAggregateFpThreshold) {
				t.Fatalf("client %q NatAggregate=%v distinctFp=%d", name, e.NatAggregate, len(wantFps[name]))
			}

			seen := map[string]bool{}
			for _, ip := range e.IPs {
				if seen[ip] {
					t.Fatalf("client %q duplicate IP %q", name, ip)
				}
				seen[ip] = true
				if !wantIPs[name][ip] {
					t.Fatalf("client %q unexpected IP %q", name, ip)
				}
			}
			if len(seen) != len(wantIPs[name]) {
				t.Fatalf("client %q got %d distinct IPs want %d", name, len(seen), len(wantIPs[name]))
			}
		}
	}
}

// --- Property: pruneOldest respects the floor and never touches aggregates ---

func TestPruneOldestProperty(t *testing.T) {
	w := newRawWriter(t)
	rng := rand.New(rand.NewSource(2))
	now := time.Now()

	for trial := 0; trial < 100; trial++ {
		if err := w.db.Exec("DELETE FROM log_entries").Error; err != nil {
			t.Fatalf("reset log_entries: %v", err)
		}
		if err := w.db.Exec("DELETE FROM agg_hourly").Error; err != nil {
			t.Fatalf("reset agg_hourly: %v", err)
		}

		// spread rows over the last 48h via the real Write+flush path so the
		// aggregate tables are populated exactly as production writes them.
		n := 1 + rng.Intn(60)
		for i := 0; i < n; i++ {
			age := time.Duration(rng.Intn(48*60)) * time.Minute
			w.Write(&LogEntry{
				Start:        now.Add(-age),
				ClientNames:  []string{"c" + strconv.Itoa(rng.Intn(3))},
				QuestionName: "q.example.com.",
				ResponseType: "RESOLVED",
				DurationMs:   int64(rng.Intn(2000)),
				Fingerprint:  model.Fingerprint{Transport: model.TransportDoT},
			})
		}
		if err := w.doDBWrite(); err != nil {
			t.Fatalf("flush: %v", err)
		}

		aggBefore := countAggHourly(w.db)

		// UTC binds in the probes: request_ts is stored UTC and compared lexically.
		floor := now.Add(-time.Duration(rng.Intn(48)) * time.Hour).UTC()

		var atOrAfterBefore int64
		w.db.Model(&logEntry{}).Where("request_ts >= ?", floor).Count(&atOrAfterBefore)

		if _, err := w.pruneOldest(floor, 1000); err != nil {
			t.Fatalf("pruneOldest: %v", err)
		}

		// invariant: rows at/after the floor are never deleted.
		var atOrAfterAfter int64
		w.db.Model(&logEntry{}).Where("request_ts >= ?", floor).Count(&atOrAfterAfter)
		if atOrAfterAfter != atOrAfterBefore {
			t.Fatalf("trial %d: prune removed rows >= floor: %d -> %d", trial, atOrAfterBefore, atOrAfterAfter)
		}

		// invariant: with limit 1000 >> n, every sub-floor row is gone.
		var belowFloor int64
		w.db.Model(&logEntry{}).Where("request_ts < ?", floor).Count(&belowFloor)
		if belowFloor != 0 {
			t.Fatalf("trial %d: %d rows older than floor survived a limit-1000 prune", trial, belowFloor)
		}

		// invariant: aggregate counts are untouched by pruning.
		if got := countAggHourly(w.db); got != aggBefore {
			t.Fatalf("trial %d: agg_hourly changed by prune: %d -> %d", trial, aggBefore, got)
		}
	}
}
