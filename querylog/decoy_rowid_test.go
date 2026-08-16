//go:build !mips && !mipsle && !mips64 && !mips64le && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package querylog

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// TestLeRowidBoundsWindowed pins the replay-window rowid floor: pre-window rows
// must be excluded from the random-draw range, otherwise every pre-window draw
// PK-scans up to the window boundary AND collapses the sample distribution onto
// the first in-window row.
func TestLeRowidBoundsWindowed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rowid.db")

	raw, err := gorm.Open(sqlite.Open(p), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}

	if err := raw.AutoMigrate(&realRow{}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	rows := []realRow{
		{RequestTS: now.Add(-30 * 24 * time.Hour), QuestionName: "old.example", QuestionType: "A"}, // rowid 1, pre-window
		{RequestTS: now.Add(-2 * time.Hour), QuestionName: "in1.example", QuestionType: "A"},       // rowid 2
		{RequestTS: now.Add(-time.Hour), QuestionName: "in2.example", QuestionType: "A"},           // rowid 3
	}
	if err := raw.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	if db, dbErr := raw.DB(); dbErr == nil {
		_ = db.Close()
	}

	ro, err := openReadOnlyPool(p, 2)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if db, dbErr := ro.DB(); dbErr == nil {
			_ = db.Close()
		}
	}()

	s := &DecoySource{ro: ro}

	minRowid, maxRowid, ok, err := s.leRowidBounds()
	if err != nil {
		t.Fatal(err)
	}

	if !ok || minRowid != 2 || maxRowid != 3 {
		t.Fatalf("bounds = [%d,%d] ok=%v, want [2,3] true (pre-window row must not widen the range)",
			minRowid, maxRowid, ok)
	}
}
