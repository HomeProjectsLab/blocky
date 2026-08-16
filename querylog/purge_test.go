package querylog

import (
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestPurgeQueryLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "querylog.db")

	dial, err := newSQLiteDialector(path)
	if err != nil {
		t.Fatalf("dialector: %v", err)
	}

	db, err := gorm.Open(dial, &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := db.AutoMigrate(&logEntry{}, &aggHourly{}, &aggDomainHourly{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Seed a few raw rows + an aggregate row.
	for i := 0; i < 5; i++ {
		if err := db.Create(&logEntry{ClientName: "c", QuestionName: "a.example.", RequestTS: time.Now()}).Error; err != nil {
			t.Fatalf("seed log_entries: %v", err)
		}
	}

	if err := db.Exec("INSERT INTO agg_hourly (hour) VALUES (?)", time.Now()).Error; err != nil {
		t.Fatalf("seed agg_hourly: %v", err)
	}

	var before int64

	db.Model(&logEntry{}).Count(&before)

	if before != 5 {
		t.Fatalf("seed count = %d, want 5", before)
	}

	// Close the seeding connection so the purge's own connection isn't blocked.
	sqlDB, _ := db.DB()
	_ = sqlDB.Close()

	if err := PurgeQueryLog(path); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// Reopen and confirm both tables are empty.
	db2, err := gorm.Open(dial, &gorm.Config{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	defer func() { s, _ := db2.DB(); _ = s.Close() }()

	var logs, aggs int64

	db2.Model(&logEntry{}).Count(&logs)
	db2.Table("agg_hourly").Count(&aggs)

	if logs != 0 || aggs != 0 {
		t.Fatalf("after purge: log_entries=%d agg_hourly=%d, want 0/0", logs, aggs)
	}
}
