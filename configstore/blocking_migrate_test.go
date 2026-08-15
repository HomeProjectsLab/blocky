package configstore

import "testing"

// oldAllowlistEntry / oldDenylistEntry are the pre-upgrade schema: the same
// tables without the Enabled column. AutoMigrate must add Enabled with a SQLite
// DEFAULT 1 so rows written before the upgrade read back Enabled==true — else a
// user's manual allow/deny rules all go dark after a version bump.
type oldAllowlistEntry struct {
	ID        uint `gorm:"primaryKey"`
	GroupName string
	Domain    string
}

func (oldAllowlistEntry) TableName() string { return "allowlist_entry" }

type oldDenylistEntry struct {
	ID        uint `gorm:"primaryKey"`
	GroupName string
	Domain    string
}

func (oldDenylistEntry) TableName() string { return "denylist_entry" }

func TestAutoMigrateBackfillsEnabled(t *testing.T) {
	db, err := openGorm(t.TempDir())
	if err != nil {
		t.Fatalf("openGorm: %v", err)
	}

	// old schema, then two rows each with no Enabled column
	if err := db.AutoMigrate(&oldAllowlistEntry{}, &oldDenylistEntry{}); err != nil {
		t.Fatalf("migrate old schema: %v", err)
	}

	if err := db.Create(&[]oldAllowlistEntry{{Domain: "a.example.com"}, {Domain: "b.example.com"}}).Error; err != nil {
		t.Fatalf("seed allows: %v", err)
	}

	if err := db.Create(&[]oldDenylistEntry{{Domain: "c.example.com"}, {Domain: "d.example.com"}}).Error; err != nil {
		t.Fatalf("seed denies: %v", err)
	}

	// upgrade: adds the Enabled column with DEFAULT 1
	if err := db.AutoMigrate(&AllowlistEntry{}, &DenylistEntry{}); err != nil {
		t.Fatalf("migrate new schema: %v", err)
	}

	var allows []AllowlistEntry
	if err := db.Order("id").Find(&allows).Error; err != nil {
		t.Fatalf("read allows: %v", err)
	}

	var denies []DenylistEntry
	if err := db.Order("id").Find(&denies).Error; err != nil {
		t.Fatalf("read denies: %v", err)
	}

	if len(allows) != 2 || len(denies) != 2 {
		t.Fatalf("want 2 allow + 2 deny rows, got %d + %d", len(allows), len(denies))
	}

	for _, e := range allows {
		if !e.Enabled {
			t.Errorf("allow %q read back disabled after migrate; existing rules would go dark", e.Domain)
		}
	}

	for _, e := range denies {
		if !e.Enabled {
			t.Errorf("deny %q read back disabled after migrate; existing rules would go dark", e.Domain)
		}
	}
}
