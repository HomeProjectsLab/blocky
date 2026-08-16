package configstore

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// An overlay-table edit (here: a deny entry) must flip Status() dirty even
// though it never touches config_blob.updated_at — otherwise the pending-apply
// banner never shows and DNS serves stale lists indefinitely.
func TestOverlayEditMarksDirty(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.MarkApplied()

	if dirty, _, _, err := s.Status(); err != nil || dirty {
		t.Fatalf("want clean after apply, got dirty=%v err=%v", dirty, err)
	}

	if _, err := s.AddDenyEntry("manual", "ads.example.com", ""); err != nil {
		t.Fatal(err)
	}

	if dirty, _, _, err := s.Status(); err != nil || !dirty {
		t.Fatalf("want dirty after overlay edit, got dirty=%v err=%v", dirty, err)
	}
}

// A customDNS overlay edit (the Local-DNS UI writing a zone) must flip Status()
// dirty so the pending-apply banner shows and the resolver rebuilds with the new
// record — the "config never applied" footgun behind the redirects-upstream report.
func TestLocalDNSZoneMarksDirty(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.MarkApplied()

	if dirty, _, _, err := s.Status(); err != nil || dirty {
		t.Fatalf("want clean after apply, got dirty=%v err=%v", dirty, err)
	}

	if err := s.SetLocalDNSZone("host.lan. 3600 IN A 10.0.0.5\n"); err != nil {
		t.Fatal(err)
	}

	if dirty, _, _, err := s.Status(); err != nil || !dirty {
		t.Fatalf("want dirty after local zone edit, got dirty=%v err=%v", dirty, err)
	}
}

// VerifyConfigDB must reject a SQLite file that is not a config database (the
// querylog.db-upload case) and accept a real config.db — without mutating either.
func TestVerifyConfigDB(t *testing.T) {
	dir := t.TempDir()

	// a real config.db passes
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	path := s.DBPath()

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if err := VerifyConfigDB(path); err != nil {
		t.Fatalf("valid config.db rejected: %v", err)
	}

	// a foreign SQLite file (no config_blob) is rejected
	other := filepath.Join(dir, "querylog.db")

	db, err := gorm.Open(sqlite.Open(other), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Exec("CREATE TABLE log_entries (id INTEGER PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}

	sqlDB, _ := db.DB()
	_ = sqlDB.Close()

	if err := VerifyConfigDB(other); err == nil {
		t.Fatal("non-config SQLite file passed VerifyConfigDB")
	}
}
