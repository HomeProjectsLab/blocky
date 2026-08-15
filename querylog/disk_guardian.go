package querylog

import (
	"context"
	"path/filepath"
	"time"

	"github.com/0xERR0R/blocky/log"
)

// Disk-pressure guardian for the sqlite query log.
//
// The raw log_entries table grows without bound; on a small appliance disk a
// full filesystem takes the whole box down. The hourly aggregate tables
// (agg_hourly, agg_domain_hourly) are written atomically in the same tx as each
// raw row (see doDBWrite -> upsertAggregates), so the dashboard statistics
// survive independently of the raw rows. This guardian keeps a target fraction
// of the DB filesystem free by deleting the OLDEST raw rows under pressure —
// never below a recent-data floor — and returns freed pages to the OS via
// incremental_vacuum. Pruned rows drop out of raw Search / fingerprint
// drill-down only; every statistic is preserved.
const (
	// diskFreeTargetFrac: fraction of the DB filesystem to keep free. Under it,
	// the guardian prunes oldest raw rows until back above it.
	//
	// ponytail: fixed 30% target + fixed cadence + fixed floor. Promote to
	// queryLog config if a box ever needs per-disk tuning.
	diskFreeTargetFrac = 0.30
	diskGuardInterval  = 5 * time.Minute
	// diskGuardMinRetain: never prune raw rows newer than this. Guarantees recent
	// Search/fingerprint data survives, and stops the guardian from wiping all
	// history when the disk is full from something OTHER than the query log
	// (deleting query rows wouldn't help there anyway).
	diskGuardMinRetain = 1 * time.Hour
	diskGuardBatch     = 20_000 // raw rows per delete step
	diskGuardMaxSteps  = 100    // bound work per tick (<= 2M rows)
)

// freeFractionFn is the free-space probe; a var so tests can simulate disk
// pressure without a real full filesystem.
//
//nolint:gochecknoglobals
var freeFractionFn = freeFraction

// diskGuardian runs the disk-pressure loop until ctx is cancelled. Started only
// for sqlite targets (see NewDatabaseWriter).
func (d *DatabaseWriter) diskGuardian(ctx context.Context) {
	dir := filepath.Dir(d.dbPath)
	ticker := time.NewTicker(diskGuardInterval)

	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.enforceDiskTarget(ctx, dir)
		}
	}
}

// enforceDiskTarget prunes oldest raw rows until the DB filesystem is at least
// diskFreeTargetFrac free, the recent-data floor is reached, or the per-tick
// work bound is hit.
func (d *DatabaseWriter) enforceDiskTarget(ctx context.Context, dir string) {
	logger := log.PrefixedLog("disk_guardian")

	free, err := freeFractionFn(dir)
	if err != nil {
		return // unsupported platform or transient stat error: skip quietly
	}

	if free >= diskFreeTargetFrac {
		return
	}

	logger.Warnf("disk %.0f%% free (< %.0f%% target) on %s; pruning oldest query-log rows — aggregate stats are preserved",
		free*100, diskFreeTargetFrac*100, dir)

	floor := time.Now().Add(-diskGuardMinRetain)

	var total int64

	for step := 0; step < diskGuardMaxSteps; step++ {
		// Stop before touching the DB if this bundle was retired (config apply):
		// after retireGrace the retire path closes these conns, so continuing would
		// hit "database is closed" and leave the goroutine spinning on a dead DB.
		if ctx.Err() != nil {
			return
		}

		deleted, err := d.pruneOldest(floor, diskGuardBatch)
		if err != nil {
			logger.Errorf("prune failed: %v", err)

			break
		}

		total += deleted
		if deleted == 0 {
			break // nothing older than the floor remains
		}

		// Return freed pages to the OS (auto_vacuum=INCREMENTAL is enabled at
		// startup), then checkpoint-truncate: under WAL the delete + vacuum only
		// shrinks the main .db file at a checkpoint, and the -wal grows during the
		// loop, so without this the re-stat below can never rise and the free-space
		// early-exit is dead (the loop would only stop at the floor or step bound).
		d.db.Exec("PRAGMA incremental_vacuum")
		d.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")

		if free, err = freeFractionFn(dir); err != nil || free >= diskFreeTargetFrac {
			break
		}
	}

	free, _ = freeFractionFn(dir)

	switch {
	case free >= diskFreeTargetFrac:
		if total > 0 {
			logger.Infof("pruned %d oldest query-log rows; disk now %.0f%% free", total, free*100)
		}
	case total == 0:
		logger.Warnf("disk %.0f%% free but no query-log rows older than %s to prune; pressure is outside the query log",
			free*100, diskGuardMinRetain)
	default:
		logger.Warnf("pruned %d rows; disk still %.0f%% free (< %.0f%%) — pressure likely outside the query log",
			total, free*100, diskFreeTargetFrac*100)
	}
}

// pruneOldest deletes up to limit of the oldest raw log_entries strictly older
// than floor and returns how many were removed. Aggregate tables are untouched.
// Serialized against the flush via d.lock (sqlite is single-writer).
func (d *DatabaseWriter) pruneOldest(floor time.Time, limit int) (int64, error) {
	d.lock.Lock()
	defer d.lock.Unlock()

	res := d.db.Exec(
		"DELETE FROM log_entries WHERE rowid IN "+
			"(SELECT rowid FROM log_entries WHERE request_ts < ? ORDER BY request_ts ASC LIMIT ?)",
		floor, limit)

	return res.RowsAffected, res.Error
}
