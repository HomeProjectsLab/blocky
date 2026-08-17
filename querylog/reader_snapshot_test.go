package querylog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// SnapshotTo must run VACUUM INTO on the READ-ONLY reader handle and produce a
// standalone, complete copy — same row count, openable as its own DB. This is the
// one risky bit: VACUUM INTO on a mode=ro connection.
func TestReaderSnapshotTo(t *testing.T) {
	w, r := writerReaderOnTemp(t)
	writeN(t, w, 25, time.Now(), false)

	want := countRows(t, r)
	if want != 25 {
		t.Fatalf("source has %d rows, expected 25", want)
	}

	dst := filepath.Join(t.TempDir(), "snap.db")
	if err := r.SnapshotTo(dst); err != nil {
		t.Fatalf("SnapshotTo on read-only handle: %v", err)
	}

	// snapshot must be a real SQLite file...
	magic, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}

	if len(magic) < 16 || string(magic[:16]) != "SQLite format 3\x00" {
		t.Fatalf("snapshot is not a SQLite database (header %q)", magic[:min(16, len(magic))])
	}

	// ...and hold every logged row when reopened as its own database.
	snapR, err := NewReader(dst)
	if err != nil {
		t.Fatalf("reopen snapshot: %v", err)
	}
	defer snapR.Close()

	if got := countRows(t, snapR); got != want {
		t.Errorf("snapshot has %d rows, source had %d", got, want)
	}
}

func countRows(t *testing.T, r *Reader) int64 {
	t.Helper()

	var n int64
	if err := r.db.Raw("SELECT COUNT(*) FROM log_entries").Scan(&n).Error; err != nil {
		t.Fatal(err)
	}

	return n
}
