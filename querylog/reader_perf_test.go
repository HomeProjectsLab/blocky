package querylog

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xERR0R/blocky/config"
)

// writerReaderOnTemp builds a sqlite writer + reader on a fresh temp file.
func writerReaderOnTemp(t *testing.T) (*DatabaseWriter, *Reader) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "q.db")

	w, err := NewDatabaseWriter(context.Background(), config.QueryLogTypeSqlite, dbPath, 7, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if sqlDB, err := w.db.DB(); err == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}

	r, err := NewReader(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = r.Close() })

	return w, r
}

func writeN(t *testing.T, w *DatabaseWriter, n int, base time.Time, decoy bool) {
	t.Helper()

	for i := 0; i < n; i++ {
		w.Write(&LogEntry{
			Start:        base.Add(-time.Duration(i) * time.Second),
			ClientIP:     "10.0.0.1",
			ClientNames:  []string{"host"},
			QuestionName: "a.example.com",
			Decoy:        decoy,
		})
	}

	if err := w.doDBWrite(); err != nil {
		t.Fatal(err)
	}
}

// The (decoy, request_ts) composite index must exist so the enrichment / noise
// window scans can seek by decoy then range on time, instead of reading every
// (mostly-decoy) row in the window.
func TestDecoyRequestTsIndexExists(t *testing.T) {
	_, r := writerReaderOnTemp(t)

	var n int64
	if err := r.db.Raw(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_decoy_request_ts'").
		Scan(&n).Error; err != nil {
		t.Fatal(err)
	}

	if n != 1 {
		t.Errorf("idx_decoy_request_ts not created (found %d)", n)
	}
}

// TotalQueries is polled every second by the dashboard; it must serve a cached
// value within the TTL rather than re-COUNT(*) the growing table each call.
func TestTotalQueriesCached(t *testing.T) {
	w, r := writerReaderOnTemp(t)
	now := time.Now()

	writeN(t, w, 10, now, false)

	first, err := r.TotalQueries()
	if err != nil {
		t.Fatal(err)
	}

	if first != 10 {
		t.Fatalf("first count = %d, want 10", first)
	}

	// add more rows, then call again immediately — still inside the TTL, so the
	// cached (pre-insert) value must come back, proving no re-scan per call.
	writeN(t, w, 5, now, false)

	second, err := r.TotalQueries()
	if err != nil {
		t.Fatal(err)
	}

	if second != first {
		t.Errorf("TotalQueries re-scanned within TTL: %d -> %d (should be cached)", first, second)
	}

	// force expiry and confirm it refreshes to the true count.
	r.totalMu.Lock()
	r.totalAt = now.Add(-2 * totalQueriesTTL)
	r.totalMu.Unlock()

	third, err := r.TotalQueries()
	if err != nil {
		t.Fatal(err)
	}

	if third != 15 {
		t.Errorf("TotalQueries after TTL = %d, want 15", third)
	}
}
