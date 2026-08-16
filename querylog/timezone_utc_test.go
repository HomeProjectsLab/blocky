package querylog

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xERR0R/blocky/config"
)

// Regression for the lexical-TZ bug: request_ts is TEXT compared lexically, so a
// row written with a non-UTC offset lands outside a UTC-bound window even when
// the instants overlap. The writer must store UTC and the readers must bind UTC.
func TestSearchFindsRowsAcrossTimezones(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "q.db")

	w, err := NewDatabaseWriter(ctx, config.QueryLogTypeSqlite, dbPath, 7, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer w.CloseDB()

	// +14:00 makes the lexical mis-order deterministic: the local date string is
	// a day AHEAD of the UTC window strings, so pre-fix the row is always missed.
	loc := time.FixedZone("UTC+14", 14*3600)
	start := time.Date(2026, 8, 16, 1, 0, 0, 0, loc) // == 2026-08-15T11:00:00Z

	w.Write(&LogEntry{Start: start, ClientIP: "192.168.1.10", QuestionName: "example.com.", QuestionType: "A"})

	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// Pin the writer half absolutely: the stored TEXT must carry a UTC offset,
	// otherwise a consistent-but-local writer+reader pair would still pass below.
	var raw string
	if err := r.db.Raw("SELECT request_ts FROM log_entries LIMIT 1").Scan(&raw).Error; err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(raw, "Z") && !strings.Contains(raw, "+00:00") {
		t.Fatalf("request_ts stored with a non-UTC offset: %q", raw)
	}

	total, items, err := r.Search(SearchFilter{
		From: start.Add(-time.Hour), // local-offset bounds; Search must bind them UTC
		To:   start.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	if total != 1 || len(items) != 1 {
		t.Fatalf("row written at %s not found in overlapping window: total=%d items=%d",
			start, total, len(items))
	}
}

// Regression for the torn maxRowid write: loadMaxRowid must assign under s.mu
// while emit workers sample concurrently. Run with -race.
func TestLoadMaxRowidRace(t *testing.T) {
	s, err := NewDecoySource(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.SeedIfEmpty(strings.NewReader("example.com\nexample.org\n")); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	for range 4 {
		wg.Add(2)

		go func() {
			defer wg.Done()

			for range 25 {
				if _, err := s.SampleList(); err != nil {
					t.Error(err)
				}
			}
		}()

		go func() {
			defer wg.Done()

			for range 25 {
				if err := s.loadMaxRowid(); err != nil {
					t.Error(err)
				}
			}
		}()
	}

	wg.Wait()
}
