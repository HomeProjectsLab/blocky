package querylog

import (
	"math"
	"strconv"
	"testing"
	"time"
)

// Phase-4 maintenance tests (blueprint §5.2/§5.3). Fold-homomorphism, purge
// durability, IP-independence and the domainset cap are already pinned in
// heuristics_test.go and are NOT duplicated here. This file adds the two gaps:
// exact device_class_signal accumulator correctness (Welford gap stats verified
// against a hand-computed ground truth, not just single==multi) and the Pi3
// row-cap guards (service_usage top-N eviction + whole-fold row bounds).

// TestClassSignalWelfordExact folds a known timestamp stream and checks the stored
// gap accumulators (gap_sum = Σg, gap_sq_sum = Σg², gap_n = count) equal the
// hand-computed consecutive gaps, and that toFeatures() recovers the exact mean /
// mean-of-squares classify() consumes. This proves the last_ts cursor reproduces
// the retired LAG-over-partition window arithmetically, across a batch boundary.
func TestClassSignalWelfordExact(t *testing.T) {
	db := openHeuristicsDB(t)
	base := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

	// Deliberately uneven gaps (seconds): 60,120,60,300,120,60,600 between 8 rows.
	offsets := []int{0, 60, 180, 240, 540, 660, 720, 1320}

	var entries []*logEntry
	for _, off := range offsets {
		entries = append(entries, &logEntry{
			RequestTS: base.Add(time.Duration(off) * time.Second), ClientName: "d", FpHash: "welf",
			QuestionName: "a.example.com", QuestionType: "A", EffectiveTLDP: "a.example.com",
		})
	}

	// Ground truth: consecutive gaps over the whole stream (first row has no prior,
	// matching LAG's NULL → n-1 gaps).
	var wantSum, wantSq float64

	var wantN int64

	for i := 1; i < len(offsets); i++ {
		g := float64(offsets[i] - offsets[i-1])
		wantSum += g
		wantSq += g * g
		wantN++
	}

	// Fold across a boundary that straddles a gap, so the cursor (not just an
	// in-batch loop) is exercised.
	if err := upsertHeuristics(db, entries[:3]); err != nil {
		t.Fatal(err)
	}

	if err := upsertHeuristics(db, entries[3:]); err != nil {
		t.Fatal(err)
	}

	s := sigOf(t, db, "welf")

	if s.GapN != wantN {
		t.Fatalf("gap_n = %d, want %d", s.GapN, wantN)
	}

	if math.Abs(s.GapSum-wantSum) > 1e-6 {
		t.Fatalf("gap_sum = %v, want %v", s.GapSum, wantSum)
	}

	if math.Abs(s.GapSqSum-wantSq) > 1e-6 {
		t.Fatalf("gap_sq_sum = %v, want %v", s.GapSqSum, wantSq)
	}

	f := s.toFeatures()
	if math.Abs(f.MeanGap-wantSum/float64(wantN)) > 1e-6 {
		t.Fatalf("MeanGap = %v, want %v", f.MeanGap, wantSum/float64(wantN))
	}

	if math.Abs(f.MeanGap2-wantSq/float64(wantN)) > 1e-6 {
		t.Fatalf("MeanGap2 = %v, want %v", f.MeanGap2, wantSq/float64(wantN))
	}
}

// TestClassSignalEmptyEtldBatchCursor pins the gap-cursor fix: a first batch made
// entirely of empty-eTLD+1 rows (single-label / *.local names) must still advance
// the stored last_ts to the batch's true last request ts, NOT leave it at the Go
// zero time. If it regressed, the next batch would seed prevTs from 0001-01-01 and
// record a ~6.4e10s first gap that dominates gap_sum/gap_sq_sum → CoV corruption.
func TestClassSignalEmptyEtldBatchCursor(t *testing.T) {
	db := openHeuristicsDB(t)
	base := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

	mk := func(off int, etld string) *logEntry {
		return &logEntry{
			RequestTS: base.Add(time.Duration(off) * time.Second), ClientName: "d", FpHash: "loc",
			QuestionName: "printer.local", QuestionType: "A", EffectiveTLDP: etld,
		}
	}

	// Batch 1: only empty-eTLD rows (gaps 60 within). Batch 2: eTLD-bearing.
	if err := upsertHeuristics(db, []*logEntry{mk(0, ""), mk(60, "")}); err != nil {
		t.Fatal(err)
	}

	if err := upsertHeuristics(db, []*logEntry{mk(120, "a.example.com"), mk(180, "a.example.com")}); err != nil {
		t.Fatal(err)
	}

	// Gaps: 60 (in b1), 60 (cross-batch), 60 (in b2) = 3 gaps, all 60s.
	s := sigOf(t, db, "loc")
	if s.GapN != 3 || math.Abs(s.GapSum-180) > 1e-6 || math.Abs(s.GapSqSum-3*3600) > 1e-6 {
		t.Fatalf("gap cursor corrupted: gap_n=%d gap_sum=%v gap_sq_sum=%v, want 3/180/10800", s.GapN, s.GapSum, s.GapSqSum)
	}
}

// TestServiceUsageTopNCap is the Pi3 growth-bomb guard for the one unbounded
// table: pruneServiceUsage must evict everything past the top serviceCapPerDevice
// by hits, per fp, keeping the hottest and dropping the coldest. Seeded directly
// (the app catalog is ~10 services, far under the cap, so the fold can't reach it)
// so the eviction SQL itself is what's tested.
func TestServiceUsageTopNCap(t *testing.T) {
	db := openHeuristicsDB(t)
	now := time.Now()

	const over = serviceCapPerDevice + 20 // 52 distinct services on one fp

	for i := 0; i < over; i++ {
		if err := db.Create(&serviceUsage{
			FpHash: "big", Service: "svc-" + strconv.Itoa(i),
			Hits: int64(i + 1), FirstSeen: now, LastSeen: now, // hits 1..52; the top 32 are 21..52
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	// A second fp under the cap must be left completely untouched.
	for i := 0; i < 5; i++ {
		if err := db.Create(&serviceUsage{
			FpHash: "small", Service: "s" + strconv.Itoa(i), Hits: 1, FirstSeen: now, LastSeen: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	if err := pruneServiceUsage(db); err != nil {
		t.Fatal(err)
	}

	var bigCount, smallCount int64
	db.Model(&serviceUsage{}).Where("fp_hash = ?", "big").Count(&bigCount)
	db.Model(&serviceUsage{}).Where("fp_hash = ?", "small").Count(&smallCount)

	if bigCount != serviceCapPerDevice {
		t.Fatalf("service_usage not capped: fp 'big' kept %d rows, want %d", bigCount, serviceCapPerDevice)
	}

	if smallCount != 5 {
		t.Fatalf("under-cap fp 'small' was disturbed: %d rows, want 5", smallCount)
	}

	// The survivors must be the HOTTEST: min surviving hits = over-cap+1 (21), so
	// the coldest (hits 1..20) were the ones evicted.
	var minHits int64
	if err := db.Model(&serviceUsage{}).Where("fp_hash = ?", "big").
		Select("MIN(hits)").Scan(&minHits).Error; err != nil {
		t.Fatal(err)
	}

	if want := int64(over - serviceCapPerDevice + 1); minHits != want {
		t.Fatalf("wrong rows evicted: min surviving hits = %d, want %d (top-N by hits)", minHits, want)
	}
}

// TestHeuristicsRowCapsAreBounded is the §5.3 "O(devices × dims), never O(rows)"
// guard: fold a large stream over a few devices and assert every per-device table
// stays proportional to device count and fixed dimensions, not to the row volume.
// A regression that keyed a table on the raw row (or dropped a cap) blows past
// these bounds long before it fills a Pi3's SD card.
func TestHeuristicsRowCapsAreBounded(t *testing.T) {
	db := openHeuristicsDB(t)
	base := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

	const (
		devices   = 4
		perDev    = 500 // 2000 raw rows total
		wantIdent = devices
	)

	fps := []string{"fpA", "fpB", "fpC", "fpD"}

	var entries []*logEntry
	for i := 0; i < perDev; i++ {
		for _, fp := range fps {
			d := string(rune('a'+i%12)) + ".example.com" // 12 distinct eTLD+1s
			entries = append(entries, &logEntry{
				RequestTS: base.Add(time.Duration(i) * time.Minute), ClientName: fp, FpHash: fp,
				QuestionName: d, QuestionType: []string{"A", "AAAA"}[i%2], EffectiveTLDP: d,
			})
		}
	}

	if err := upsertHeuristics(db, entries); err != nil {
		t.Fatal(err)
	}

	rows := func(m any) int64 {
		var n int64

		db.Model(m).Count(&n)

		return n
	}

	total := int64(devices * perDev) // 2000 raw rows folded

	// identity / class_signal: exactly one row per device, never per raw row.
	if got := rows(&deviceIdentity{}); got != wantIdent {
		t.Fatalf("device_identity = %d rows, want %d (one per fp, not per row)", got, wantIdent)
	}

	if got := rows(&deviceClassSignal{}); got != wantIdent {
		t.Fatalf("device_class_signal = %d rows, want %d", got, wantIdent)
	}

	// presence: ≤168 cells/fp, and far below the raw row count.
	if got := rows(&devicePresence{}); got > int64(devices*168) || got >= total {
		t.Fatalf("device_presence = %d rows, expected ≤%d and < %d raw rows", got, devices*168, total)
	}

	// domainset: hard-capped at classIoTMaxDomains+1 per fp regardless of the 12
	// distinct eTLD+1s seen.
	if got := rows(&deviceClassDomainset{}); got > int64(devices*(classIoTMaxDomains+1)) {
		t.Fatalf("device_class_domainset = %d rows, exceeds cap %d", got, devices*(classIoTMaxDomains+1))
	}

	var dsBig int64
	db.Model(&deviceClassDomainset{}).Where("fp_hash = ?", "fpA").Count(&dsBig)

	if dsBig > classIoTMaxDomains+1 {
		t.Fatalf("domainset for one fp = %d, exceeds %d", dsBig, classIoTMaxDomains+1)
	}
}

// TestEvictStaleHeuristics pins the growth bound: keys silent past the cutoff are
// evicted from every auto table, while fresh keys and manual device_class
// overrides survive.
func TestEvictStaleHeuristics(t *testing.T) {
	db := openHeuristicsDB(t)
	s := &DecoySource{db: db}

	now := time.Now().UTC()
	old := now.Add(-2 * heuristicsStaleAfter)

	for key, seen := range map[string]time.Time{"stale": old, "fresh": now} {
		if err := db.Create(&deviceIdentity{FpHash: key, FirstSeen: seen, LastSeen: seen}).Error; err != nil {
			t.Fatal(err)
		}

		if err := db.Create(&deviceClassSignal{FpHash: key, N: 1}).Error; err != nil {
			t.Fatal(err)
		}

		if err := db.Create(&fpBinding{ClientName: "c-" + key, FpHash: key, FirstSeen: seen, LastSeen: seen}).Error; err != nil {
			t.Fatal(err)
		}
	}

	// stale key carries a manual override: the class row must be preserved
	if err := db.Create(&deviceClass{FpHash: "stale", Class: "camera", Override: "server", UpdatedAt: old}).Error; err != nil {
		t.Fatal(err)
	}

	if err := s.evictStaleHeuristics(now.Add(-heuristicsStaleAfter)); err != nil {
		t.Fatal(err)
	}

	count := func(q string, args ...any) (n int64) {
		t.Helper()

		if err := db.Raw(q, args...).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}

		return n
	}

	if n := count(`SELECT COUNT(*) FROM device_identity WHERE fp_hash = 'stale'`); n != 0 {
		t.Fatalf("stale device_identity rows = %d, want 0", n)
	}

	if n := count(`SELECT COUNT(*) FROM device_class_signal WHERE fp_hash = 'stale'`); n != 0 {
		t.Fatalf("stale device_class_signal rows = %d, want 0", n)
	}

	if n := count(`SELECT COUNT(*) FROM fp_binding WHERE fp_hash = 'stale'`); n != 0 {
		t.Fatalf("stale fp_binding rows = %d, want 0", n)
	}

	if n := count(`SELECT COUNT(*) FROM device_class WHERE fp_hash = 'stale' AND override = 'server'`); n != 1 {
		t.Fatalf("manual override rows = %d, want 1 (must survive eviction)", n)
	}

	if n := count(`SELECT COUNT(*) FROM device_identity WHERE fp_hash = 'fresh'`); n != 1 {
		t.Fatalf("fresh device_identity rows = %d, want 1", n)
	}

	if n := count(`SELECT COUNT(*) FROM device_class_signal WHERE fp_hash = 'fresh'`); n != 1 {
		t.Fatalf("fresh device_class_signal rows = %d, want 1", n)
	}
}
