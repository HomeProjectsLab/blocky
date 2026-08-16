package querylog

import (
	"fmt"
	"time"

	"github.com/0xERR0R/blocky/log"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// PurgeQueryLog deletes ALL logged queries (log_entries) and their derived hourly
// aggregate tables from the sqlite query log, on a short-lived read-write
// connection. `DELETE FROM <table>` with no WHERE uses SQLite's truncate
// optimisation (no per-row scan), so it is fast even on a multi-million-row log.
//
// A full VACUUM to shrink the file is intentionally NOT run here: VACUUM needs an
// exclusive lock the live reader/decoy connection pools hold, and the freed pages
// are reused as new queries arrive. Config, blocklists, decoy corpus and client
// identity/class are untouched — only the query history and its stats are cleared.
func PurgeQueryLog(target string) error {
	dialector, err := newSQLiteDialector(target)
	if err != nil {
		return err
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: gormlogger.New(log.Log(), gormlogger.Config{
			SlowThreshold: time.Minute,
			LogLevel:      gormlogger.Warn,
			Colorful:      false,
		}),
	})
	if err != nil {
		return fmt.Errorf("can't open query log for purge: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("can't access query log connection: %w", err)
	}

	defer func() { _ = sqlDB.Close() }()

	sqlDB.SetMaxOpenConns(1) // one writer is all the deletes need

	// Order doesn't matter (no FKs); truncate-optimised deletes of the raw log and
	// both aggregate rollups.
	for _, table := range []string{"log_entries", "agg_hourly", "agg_domains_hourly"} {
		if err := db.Exec("DELETE FROM " + table).Error; err != nil {
			return fmt.Errorf("purge %s: %w", table, err)
		}
	}

	// Fold the deletes' WAL frames back so a stale -wal doesn't linger for readers.
	// Best effort: a concurrent writer may keep it from fully truncating.
	db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")

	return nil
}
