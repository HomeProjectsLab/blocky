package querylog

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xERR0R/blocky/config"

	"gorm.io/gorm"
)

// openHeuristicsDB opens a sqlite gorm DB with the durable heuristics tables
// migrated (single writer conn, like production).
func openHeuristicsDB(t *testing.T) *gorm.DB {
	t.Helper()

	dialector, err := newSQLiteDialector(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}

	db, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}

	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = sqlDB.Close() })
	}

	if err := db.AutoMigrate(heuristicsTables...); err != nil {
		t.Fatal(err)
	}

	return db
}

// beaconEntries builds n periodic real queries for one (name, fp) over domains.
func beaconEntries(name, fp string, domains []string, n int, start time.Time, period time.Duration) []*logEntry {
	out := make([]*logEntry, 0, n)
	for i := 0; i < n; i++ {
		d := domains[i%len(domains)]
		out = append(out, &logEntry{
			RequestTS: start.Add(time.Duration(i) * period), ClientName: name, FpHash: fp,
			QuestionName: d, QuestionType: "A", EffectiveTLDP: d,
		})
	}

	return out
}

func sigOf(t *testing.T, db *gorm.DB, key string) deviceClassSignal {
	t.Helper()

	var s deviceClassSignal
	if err := db.Where("fp_hash = ?", key).First(&s).Error; err != nil {
		t.Fatalf("read signal %q: %v", key, err)
	}

	return s
}

// TestHeuristicsFoldHomomorphism: folding a stream as one batch must equal folding
// it as several boundary-straddling batches — including the order-dependent Welford
// gap stats and the capped domainset. This proves the cursor design retired the
// LAG scan without changing results.
func TestHeuristicsFoldHomomorphism(t *testing.T) {
	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	stream := beaconEntries("iot", "aaaa1111", []string{"telemetry.tuya.com", "time.nist.gov"}, 40, base, 5*time.Minute)

	single := openHeuristicsDB(t)
	if err := upsertHeuristics(single, stream); err != nil {
		t.Fatal(err)
	}

	multi := openHeuristicsDB(t)

	for _, cut := range [][2]int{{0, 7}, {7, 7}, {7, 21}, {21, 40}} { // uneven, one empty-straddle
		if err := upsertHeuristics(multi, stream[cut[0]:cut[1]]); err != nil {
			t.Fatal(err)
		}
	}

	a, b := sigOf(t, single, "aaaa1111"), sigOf(t, multi, "aaaa1111")

	if a.N != b.N || a.QtypeMask != b.QtypeMask || a.Domains != b.Domains || a.GapN != b.GapN {
		t.Fatalf("counter mismatch: single=%+v multi=%+v", a, b)
	}

	if abs(a.GapSum-b.GapSum) > 1e-6 || abs(a.GapSqSum-b.GapSqSum) > 1e-6 {
		t.Fatalf("gap stats mismatch: single sum=%v sq=%v; multi sum=%v sq=%v",
			a.GapSum, a.GapSqSum, b.GapSum, b.GapSqSum)
	}

	if a.toFeatures().classify() != b.toFeatures().classify() {
		t.Fatalf("classify mismatch: %s vs %s", a.toFeatures().classify(), b.toFeatures().classify())
	}

	if a.toFeatures().classify() != ClassIoT {
		t.Fatalf("periodic 2-domain beacon should be iot, got %s", a.toFeatures().classify())
	}

	// domainset cap: never more than classIoTMaxDomains+1 rows for the fp.
	var dsCount int64
	single.Model(&deviceClassDomainset{}).Where("fp_hash = ?", "aaaa1111").Count(&dsCount)

	if dsCount > classIoTMaxDomains+1 {
		t.Fatalf("domainset exceeded cap: %d rows", dsCount)
	}
}

// TestHeuristicsDomainsetCap: a device touching many distinct eTLD+1s never stores
// a 10th (classIoTMaxDomains+1) row, and domains saturates at the cap.
func TestHeuristicsDomainsetCap(t *testing.T) {
	db := openHeuristicsDB(t)
	base := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

	var entries []*logEntry
	for i := 0; i < 20; i++ {
		d := string(rune('a'+i)) + ".example.com"
		entries = append(entries, &logEntry{
			RequestTS: base.Add(time.Duration(i) * time.Minute), ClientName: "ws", FpHash: "wsfp",
			QuestionName: d, QuestionType: "A", EffectiveTLDP: d,
		})
	}

	// fold in two halves to exercise the cross-batch cap gate
	if err := upsertHeuristics(db, entries[:9]); err != nil {
		t.Fatal(err)
	}

	if err := upsertHeuristics(db, entries[9:]); err != nil {
		t.Fatal(err)
	}

	var dsCount int64
	db.Model(&deviceClassDomainset{}).Where("fp_hash = ?", "wsfp").Count(&dsCount)

	if dsCount != classIoTMaxDomains+1 {
		t.Fatalf("domainset should cap at %d, got %d", classIoTMaxDomains+1, dsCount)
	}

	if got := sigOf(t, db, "wsfp").Domains; got != classIoTMaxDomains+1 {
		t.Fatalf("domains column should saturate at %d, got %d", classIoTMaxDomains+1, got)
	}
}

// TestHeuristicsFingerprintIPIndependence: the same fp under two client_names/IPs
// collapses to ONE device_identity (hits summed) with TWO fp_binding rows; two
// distinct fps stay two identities (NAT/FpCount preserved).
func TestHeuristicsFingerprintIPIndependence(t *testing.T) {
	db := openHeuristicsDB(t)
	base := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

	// same fp, two names (a DHCP/roam rename)
	e := beaconEntries("10.0.0.5", "fp-shared", []string{"a.com"}, 3, base, time.Minute)
	e = append(e, beaconEntries("10.0.0.9", "fp-shared", []string{"a.com"}, 2, base.Add(time.Hour), time.Minute)...)
	// a second distinct device under its own fp
	e = append(e, beaconEntries("10.0.0.9", "fp-other", []string{"b.com"}, 4, base, time.Minute)...)

	if err := upsertHeuristics(db, e); err != nil {
		t.Fatal(err)
	}

	var id deviceIdentity
	if err := db.Where("fp_hash = ?", "fp-shared").First(&id).Error; err != nil {
		t.Fatal(err)
	}

	if id.Hits != 5 {
		t.Fatalf("shared fp identity should sum hits across names: got %d want 5", id.Hits)
	}

	var bindingsForFp, identities int64
	db.Model(&fpBinding{}).Where("fp_hash = ?", "fp-shared").Count(&bindingsForFp)
	db.Model(&deviceIdentity{}).Count(&identities)

	if bindingsForFp != 2 {
		t.Fatalf("shared fp should have 2 client bindings, got %d", bindingsForFp)
	}

	if identities != 2 {
		t.Fatalf("two distinct fps => 2 identities, got %d", identities)
	}
}

// TestHeuristicsSurvivePurge is the durability contract: fold, purge the raw log,
// and assert the heuristics tables are untouched AND the class scorer still
// advances from the accumulator (never the emptied log_entries) — the exact bug
// this layer fixes.
func TestHeuristicsSurvivePurge(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "q.db")

	w, err := NewDatabaseWriter(context.Background(), config.QueryLogTypeSqlite, dbPath, 7, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// an IoT-shaped device, written through the real fold path
	base := time.Now().Add(-6 * time.Hour)
	for i := 0; i < 40; i++ {
		w.Write(&LogEntry{
			Start:        base.Add(time.Duration(i) * 5 * time.Minute),
			ClientNames:  []string{"sensor"},
			QuestionName: []string{"telemetry.tuya.com", "time.nist.gov"}[i%2],
			QuestionType: "A",
			ResponseType: "RESOLVED",
		})
	}

	if err := w.doDBWrite(); err != nil {
		t.Fatal(err)
	}

	src, err := NewDecoySource(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := src.RefreshClientClasses(); err != nil {
		t.Fatal(err)
	}

	classBefore, err := src.ClientClass("sensor")
	if err != nil {
		t.Fatal(err)
	}

	if classBefore != ClassIoT {
		t.Fatalf("precondition: sensor should classify iot, got %s", classBefore)
	}

	var sigBefore, presBefore int64
	src.db.Model(&deviceClassSignal{}).Count(&sigBefore)
	src.db.Model(&devicePresence{}).Count(&presBefore)

	// close the second writer so the purge's short-lived connection isn't contended
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}

	if sqlDB, e := w.db.DB(); e == nil {
		_ = sqlDB.Close()
	}

	if err := PurgeQueryLog(dbPath); err != nil {
		t.Fatal(err)
	}

	// reopen and assert: raw log gone, heuristics intact, scorer still advances
	src2, err := NewDecoySource(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = src2.Close() })

	var rawRows, sigAfter, presAfter int64
	src2.db.Table("log_entries").Count(&rawRows)
	src2.db.Model(&deviceClassSignal{}).Count(&sigAfter)
	src2.db.Model(&devicePresence{}).Count(&presAfter)

	if rawRows != 0 {
		t.Fatalf("purge should empty log_entries, got %d rows", rawRows)
	}

	if sigAfter != sigBefore || presAfter != presBefore {
		t.Fatalf("heuristics must survive purge: signal %d->%d presence %d->%d",
			sigBefore, sigAfter, presBefore, presAfter)
	}

	// The scorer reads the DURABLE accumulator, not the emptied log: class holds.
	if err := src2.RefreshClientClasses(); err != nil {
		t.Fatal(err)
	}

	classAfter, err := src2.ClientClass("sensor")
	if err != nil {
		t.Fatal(err)
	}

	if classAfter != ClassIoT {
		t.Fatalf("class reverted after purge (the bug): got %s, want iot", classAfter)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}

	return f
}
