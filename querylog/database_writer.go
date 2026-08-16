package querylog

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm/logger"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/log"
	"github.com/hashicorp/go-multierror"

	"github.com/0xERR0R/blocky/util"

	"golang.org/x/net/publicsuffix"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type logEntry struct {
	RequestTS     time.Time `gorm:"not null;index;index:idx_client_name_request_ts,priority:2;index:idx_decoy_request_ts,priority:2"`
	ClientIP      string
	ClientName    string `gorm:"index;index:idx_client_name_request_ts,priority:1"`
	DurationMs    int64
	Reason        string
	ResponseType  string `gorm:"index"`
	QuestionType  string
	QuestionName  string `gorm:"index"`
	EffectiveTLDP string // indexed by idx_log_entries_etldp_ts, built in the background (buildDeferredIndexes)
	Answer        string
	ResponseCode  string
	Hostname      string
	Transport     string `gorm:"index"`
	FpHash        string `gorm:"index"`
	DoHUserAgent  string `gorm:"column:doh_user_agent"`
	SNI           string `gorm:"column:sni"`
	EDNSUDPSize   uint16 `gorm:"column:edns_udp_size"`
	EDNSOptCodes  string `gorm:"column:edns_opt_codes"` // wire order, e.g. "10,8,12"
	Decoy         bool   `gorm:"index;index:idx_decoy_request_ts,priority:1"`
	DecoySource   string `gorm:"index"` // provenance label for decoys (empty for real rows)
	FpDetail      string // JSON with the per-query fingerprint noise
}

// fpDetail is the JSON layout of logEntry.FpDetail.
type fpDetail struct {
	SrcPort     uint16 `json:"srcport"`
	TLSVersion  uint16 `json:"tlsversion"`
	TLSCipher   uint16 `json:"tlscipher"`
	ALPN        string `json:"alpn"`
	MsgID       uint16 `json:"msgid"`
	QClass      uint16 `json:"qclass"`
	RD          bool   `json:"rd"`
	CD          bool   `json:"cd"`
	AD          bool   `json:"ad"`
	DO          bool   `json:"do"`
	HadEDNS0    bool   `json:"hadEdns0"`
	EDNSVersion uint8  `json:"ednsVersion"`
	HasCookie   bool   `json:"hasCookie"`
	Mixed0x20   bool   `json:"mixed0x20"`
}

type DatabaseWriter struct {
	db               *gorm.DB
	logRetentionDays uint64
	pendingEntries   []*logEntry
	lock             sync.RWMutex
	dbFlushPeriod    time.Duration
	// aggregate is true for sqlite targets: the hourly aggregate tables (see
	// aggregates.go) back the /api/ui stats reader, which is sqlite-only.
	aggregate bool
	// dbPath is the sqlite file path (empty for remote DBs); the disk guardian
	// stats its directory to keep the filesystem below the free-space target.
	dbPath string
	// checkpointBusy counts flush TRUNCATE checkpoints that returned BUSY or left
	// WAL frames uncheckpointed (a reader held a mark) — the WAL-starvation signal.
	// atomic.Uint64 self-aligns on 32-bit ARM/386 (a plain uint64 field here is
	// misaligned and atomic ops on it panic).
	checkpointBusy atomic.Uint64
}

func NewDatabaseWriter(ctx context.Context, dbType config.QueryLogType, target string, logRetentionDays uint64,
	dbFlushPeriod time.Duration,
) (*DatabaseWriter, error) {
	switch dbType { //nolint:exhaustive // non-database query-log types are handled in GetQueryLoggingWriter
	case config.QueryLogTypeMysql:
		return newDatabaseWriter(ctx, mysql.Open(target), logRetentionDays, dbFlushPeriod, dbType)
	case config.QueryLogTypePostgresql, config.QueryLogTypeTimescale:
		return newDatabaseWriter(ctx, postgres.Open(target), logRetentionDays, dbFlushPeriod, dbType)
	case config.QueryLogTypeSqlite:
		dialector, err := newSQLiteDialector(target)
		if err != nil {
			return nil, err
		}

		w, err := newDatabaseWriter(ctx, dialector, logRetentionDays, dbFlushPeriod, dbType)
		if err != nil {
			return nil, err
		}

		// keep the DB filesystem below the free-space target by pruning oldest
		// raw rows (stats live in the aggregate tables and are preserved)
		w.dbPath = target
		go w.diskGuardian(ctx)

		return w, nil
	}

	return nil, fmt.Errorf("incorrect database type provided: %s", dbType)
}

func newDatabaseWriter(ctx context.Context, target gorm.Dialector, logRetentionDays uint64,
	dbFlushPeriod time.Duration, dbType config.QueryLogType,
) (*DatabaseWriter, error) {
	db, err := gorm.Open(target, &gorm.Config{
		Logger: logger.New(
			log.Log(),
			logger.Config{
				SlowThreshold:             time.Minute,
				LogLevel:                  logger.Warn,
				IgnoreRecordNotFoundError: false,
				Colorful:                  false,
			}),
	})
	if err != nil {
		return nil, fmt.Errorf("can't create database connection: %w", err)
	}

	// SQLite is a single local file: a write holds an exclusive lock on the whole
	// database, so letting the pool open several connections only turns blocky's own
	// concurrent access (the periodic flush vs. the retention cleanup) into
	// SQLITE_BUSY errors. Serialize through one connection instead; external readers
	// use their own connections and are unaffected.
	if dbType == config.QueryLogTypeSqlite {
		sqlDB, err := db.DB()
		if err != nil {
			return nil, fmt.Errorf("can't access sqlite connection pool: %w", err)
		}

		sqlDB.SetMaxOpenConns(1)

		// The one-time INCREMENTAL auto_vacuum apply (a whole-file VACUUM) and the
		// heavy etldp index build run OFF this synchronous boot/apply path in
		// buildDeferredIndexes — on a multi-GB appliance DB each is minutes of write
		// lock that would block NewServer, DNS and the UI readers. See that method.
	}

	// Migrate the schema
	if err := databaseMigration(db, dbType, logRetentionDays); err != nil {
		return nil, fmt.Errorf("can't perform auto migration: %w", err)
	}

	w := &DatabaseWriter{
		db:               db,
		logRetentionDays: logRetentionDays,
		dbFlushPeriod:    dbFlushPeriod,
		aggregate:        dbType == config.QueryLogTypeSqlite,
	}

	go w.periodicFlush(ctx)

	if dbType == config.QueryLogTypeSqlite {
		go w.buildDeferredIndexes(ctx)
	}

	return w, nil
}

// heavyMaintenanceCeilingBytes gates ONLY the one-time VACUUM (see the SIZE GUARD
// in buildDeferredIndexes). A var, not a const, purely so the plan test can lower
// it to force the over-ceiling branch and assert the etldp index still builds.
//
//nolint:gochecknoglobals // test seam for the size guard
var heavyMaintenanceCeilingBytes int64 = 256 << 20 // 256 MiB

// buildDeferredIndexes runs the heavy one-time sqlite maintenance (INCREMENTAL
// auto_vacuum apply + the etldp composite index) OFF the synchronous boot/apply
// path, so NewServer returns and the box serves DNS + reads immediately. Queries
// touching effective_tldp table-scan until the index lands: slower, not broken.
//
// Held under d.lock for the full build: flushes/prunes queue behind it (no batch
// loss), and Write() blocks, so the resolver's logChan fills and it drops intake
// at its existing non-blocking send (query_logging_resolver.go:246) — bounded,
// already logged, DNS never touched. A second connection is NOT used on purpose:
// CREATE INDEX/VACUUM hold an exclusive write lock for their whole duration, so a
// flush on a second conn would exhaust busy_timeout and doDBWrite would clear the
// batch on the error (silent loss). Serializing on d.lock makes flushes queue.
//
// ponytail: SQLite has no CREATE INDEX CONCURRENTLY, so reader latency still
// degrades from SD IO *during* this one-shot build — unavoidable; but it is one
// event, boot no longer blocks for minutes, and DNS/config stay live throughout.
func (d *DatabaseWriter) buildDeferredIndexes(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	logger := log.PrefixedLog("database_writer")

	d.lock.Lock()
	defer d.lock.Unlock()

	logger.Info("deferred query-log maintenance starting")

	// SIZE GUARD: only the one-time VACUUM (a whole-file rewrite) is the OOM risk on a
	// low-RAM box — it can never commit before the container is OOM-killed, rolling
	// the work back and re-running it every boot (a crash loop that pins the disk at
	// write ceiling under an exclusive lock). The etldp CREATE INDEX is NOT gated: its
	// sort spills to disk, so peak RAM stays bounded (288ms on the 642MB backup with a
	// 2MB cache), and it backs the decoy emit hot path — bundling it behind the VACUUM
	// ceiling silently full-scanned the decoy=0 partition per emit on every big box.
	var pageCount, pageSize int64

	pcErr := d.db.Raw("PRAGMA page_count").Scan(&pageCount).Error
	psErr := d.db.Raw("PRAGMA page_size").Scan(&pageSize).Error

	dbBytes := pageCount * pageSize

	// Fail CLOSED: if the size probe errored (dbBytes==0), treat as over the ceiling
	// and skip the VACUUM — failing open re-ran the OOM VACUUM this guard exists to stop.
	overCeiling := pcErr != nil || psErr != nil || dbBytes == 0 || dbBytes > heavyMaintenanceCeilingBytes

	if overCeiling {
		logger.Warnf("query log is %d MiB (over the %d MiB ceiling, or size unreadable) — skipping the one-time VACUUM to avoid a low-RAM stall/crash-loop; the etldp index is still built",
			dbBytes>>20, heavyMaintenanceCeilingBytes>>20)
	} else {
		// One-time: apply INCREMENTAL auto_vacuum (the disk guardian's incremental_vacuum
		// needs it). VACUUM rewrites the whole file; gated so every later boot is a cheap
		// PRAGMA read once mode==2.
		var mode int

		d.db.Raw("PRAGMA auto_vacuum").Scan(&mode)

		if mode != 2 {
			d.db.Exec("PRAGMA auto_vacuum = INCREMENTAL")

			logger.WithField("op", "vacuum").WithField("db_mib", dbBytes>>20).
				Info("running one-time VACUUM (auto_vacuum=INCREMENTAL migration)")

			if err := d.db.Exec("VACUUM").Error; err != nil {
				logger.Errorf("deferred VACUUM failed: %v", err)
			} else {
				logger.WithField("op", "vacuum").Info("one-time VACUUM done")
			}
		}
	}

	// The heavy composite index, always built (see SIZE GUARD). IF NOT EXISTS =
	// idempotent/resumable across restarts (no HasIndex needed; a no-op once built).
	const stmt = "CREATE INDEX IF NOT EXISTS idx_log_entries_etldp_ts " +
		"ON log_entries (effective_tldp, request_ts)"

	if err := d.db.Exec(stmt).Error; err != nil {
		logger.Errorf("deferred index build failed: %v", err)

		return
	}

	// Fold the build's WAL frames into the main DB so readers don't inherit a giant
	// -wal from the index write.
	d.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")

	logger.WithField("op", "index").WithField("index", "idx_log_entries_etldp_ts").
		Info("deferred query-log maintenance done")
}

func databaseMigration(db *gorm.DB, dbType config.QueryLogType, logRetentionDays uint64) error {
	if err := db.AutoMigrate(&logEntry{}); err != nil {
		return fmt.Errorf("failed to auto-migrate database schema for querylog: %w", err)
	}

	tableName := db.NamingStrategy.TableName(reflect.TypeFor[logEntry]().Name())

	// create unmapped primary key
	switch dbType { //nolint:exhaustive // only database-backed targets reach migration
	case config.QueryLogTypeSqlite:
		// SQLite gives every table an implicit auto-incrementing rowid that already
		// acts as the primary key, so unlike the other targets no extra id column is
		// added here (and SQLite cannot ALTER TABLE ... ADD a PRIMARY KEY column).

		// sqlite additionally maintains the hourly aggregate tables for the UI stats
		// API, the persistent visited-domains noise corpus (decoy source, T3), and the
		// durable fingerprint-keyed heuristics tables (folded in doDBWrite, purge-immune).
		migrate := append([]any{&aggHourly{}, &aggDomainHourly{}, &noiseCorpus{}}, heuristicsTables...)
		if err := db.AutoMigrate(migrate...); err != nil {
			return fmt.Errorf("failed to auto-migrate aggregate tables for querylog: %w", err)
		}

	case config.QueryLogTypeMysql:
		tx := db.Exec("ALTER TABLE `" + tableName + "` ADD `id` INT PRIMARY KEY AUTO_INCREMENT")
		if tx.Error != nil {
			// mysql doesn't support "add column if not exist"
			if strings.Contains(tx.Error.Error(), "1060") {
				// error 1060: duplicate column name
				// ignore it
				return nil
			}

			return tx.Error
		}

	case config.QueryLogTypePostgresql:
		return db.Exec("ALTER TABLE " + tableName + " ADD column if not exists id bigserial primary key").Error

	case config.QueryLogTypeTimescale:
		requestTSColName := db.NamingStrategy.ColumnName(reflect.TypeFor[logEntry]().Name(), "RequestTS")

		// Create a Timescale hypertable
		tx := db.Exec(`SELECT create_hypertable(
			'` + tableName + `',
			by_range('` + requestTSColName + `'),
			if_not_exists => TRUE
		)`)
		if tx.Error != nil {
			return tx.Error
		}

		// Create a retention policy for the hypertable
		tx = db.Exec(`SELECT add_retention_policy(
			'` + tableName + `',
			drop_after => INTERVAL '` + strconv.FormatUint(logRetentionDays, 10) + ` days',
			if_not_exists => TRUE
		)`)
		if tx.Error != nil {
			return tx.Error
		}
	}

	return nil
}

func (d *DatabaseWriter) periodicFlush(ctx context.Context) {
	ticker := time.NewTicker(d.dbFlushPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := d.doDBWrite()

			util.LogOnError(ctx, "can't write entries to the database: ", err)

		case <-ctx.Done():
			// final flush so short-lived runs don't lose the buffered entries;
			// ctx is cancelled, so log via a fresh context
			err := d.doDBWrite()

			// Deliberately detached: ctx is already cancelled here, so deriving
			// from it would drop the shutdown log line we specifically want.
			//nolint:contextcheck // see above
			util.LogOnError(context.Background(), "can't write entries to the database on shutdown: ", err)

			return
		}
	}
}

// Flush synchronously persists all pending entries. Server.Stop calls this
// through the resolver chain so process exit can't race the async flush
// goroutine (ctx-cancel flush alone lost entries on fast shutdowns).
func (d *DatabaseWriter) Flush() error {
	return d.doDBWrite()
}

func (d *DatabaseWriter) Write(entry *LogEntry) {
	domain := util.ExtractDomainOnly(entry.QuestionName)
	eTLD, _ := publicsuffix.EffectiveTLDPlusOne(domain)

	fp := &entry.Fingerprint

	optCodes := make([]string, len(fp.EDNSOptCodes))
	for i, c := range fp.EDNSOptCodes {
		optCodes[i] = strconv.FormatUint(uint64(c), 10)
	}

	detail, err := json.Marshal(fpDetail{
		SrcPort:     fp.SrcPort,
		TLSVersion:  fp.TLSVersion,
		TLSCipher:   fp.TLSCipher,
		ALPN:        fp.ALPN,
		MsgID:       fp.MsgID,
		QClass:      fp.QClass,
		RD:          fp.RD,
		CD:          fp.CD,
		AD:          fp.AD,
		DO:          fp.DO,
		HadEDNS0:    fp.HadEDNS0,
		EDNSVersion: fp.EDNSVersion,
		HasCookie:   fp.HasCookie,
		Mixed0x20:   fp.Mixed0x20,
	})
	util.LogOnError(context.Background(), "can't marshal fingerprint detail: ", err)

	e := &logEntry{
		// UTC on write: the sqlite driver stores time.Time as TEXT with the value's
		// own offset and compares lexically, so mixed local/UTC offsets mis-order
		// every request_ts window filter (readers bind UTC to match).
		// ponytail: forward-only fix — pre-existing local-offset rows are NOT
		// migrated (a boot-time rewrite crash-looped this box before); they age out
		// via retention within a few days and self-heal.
		RequestTS:     entry.Start.UTC(),
		ClientIP:      entry.ClientIP,
		ClientName:    strings.Join(entry.ClientNames, "; "),
		DurationMs:    entry.DurationMs,
		Reason:        entry.ResponseReason,
		ResponseType:  entry.ResponseType,
		QuestionType:  entry.QuestionType,
		QuestionName:  domain,
		EffectiveTLDP: eTLD,
		Answer:        entry.Answer,
		ResponseCode:  entry.ResponseCode,
		Hostname:      entry.BlockyInstance,
		Transport:     fp.Transport.String(),
		FpHash:        fp.Hash(),
		DoHUserAgent:  fp.UserAgent,
		SNI:           fp.SNI,
		EDNSUDPSize:   fp.EDNSUDPSize,
		EDNSOptCodes:  strings.Join(optCodes, ","),
		Decoy:         entry.Decoy,
		DecoySource:   entry.DecoySource,
		FpDetail:      string(detail),
	}

	d.lock.Lock()
	defer d.lock.Unlock()

	d.pendingEntries = append(d.pendingEntries, e)
}

// CloseDB closes the underlying database connection so a retired writer doesn't
// leak an FD (+ the -wal/-shm files and gorm's session cache) on every config
// apply. Deliberately NOT named Close: the query-log resolver's ctx-cancel path
// (closeWriter) closes io.Closer writers immediately, but the DB must stay open
// until AFTER the retire/shutdown flush — closing early would make that flush hit
// a closed connection. Callers close it via the bundle's post-flush path.
func (d *DatabaseWriter) CloseDB() error {
	sqlDB, err := d.db.DB()
	if err != nil {
		return err
	}

	return sqlDB.Close()
}

func (d *DatabaseWriter) CleanUp() {
	deletionDate := time.Now().AddDate(0, 0, int(-d.logRetentionDays)) //nolint:gosec // G115: correct via two's complement

	logger := log.PrefixedLog("database_writer")
	logger.Debugf("deleting log entries with request_ts < %s", deletionDate)

	// Batch the retention delete via pruneOldest (takes d.lock, caps the txn size,
	// pauses between steps) instead of one unbounded DELETE: a large backlog in a
	// single txn holds the sole sqlite writer for seconds and spikes the WAL, which
	// is the lock-contention that starves the flush. diskGuardMaxSteps bounds the
	// work per call; the periodic cleanup resumes any remainder next tick.
	var total int64

	for step := 0; step < diskGuardMaxSteps; step++ {
		deleted, err := d.pruneOldest(deletionDate, diskGuardBatch)
		if err != nil {
			logger.Errorf("retention cleanup failed: %v", err)

			return
		}

		if deleted == 0 {
			break
		}

		total += deleted

		time.Sleep(diskGuardStepPause)
	}

	if total > 0 {
		logger.WithField("deleted", total).WithField("retention_days", d.logRetentionDays).
			Info("query-log retention cleanup pruned expired rows")
	}

	// Aggregate retention (sqlite-only tables): agg_hourly/agg_domains_hourly had
	// none, so the rollups grew forever. hour is the PK prefix → indexed range
	// delete; same cutoff as the raw log. UTC bind: hour is stored UTC, compared
	// lexically. Serialized against the flush via d.lock (mirrors pruneOldest).
	if d.aggregate {
		d.lock.Lock()
		for _, table := range []string{"agg_hourly", "agg_domains_hourly"} {
			if err := d.db.Exec("DELETE FROM "+table+" WHERE hour < ?", deletionDate.UTC()).Error; err != nil {
				logger.Errorf("aggregate retention cleanup failed (%s): %v", table, err)
			}
		}
		d.lock.Unlock()
	}
}

// maxRetainedEntries caps the flush retry buffer (pendingEntries kept across
// failed flushes): newest wins, oldest dropped. ~10k rows is a few MB — hours of
// household traffic — well within the Pi's RAM while a fault persists.
const maxRetainedEntries = 10_000

func (d *DatabaseWriter) doDBWrite() error {
	if err := d.flushPending(); err != nil {
		return err // partial failure: keep the old behaviour of skipping the checkpoint
	}

	// Bound the -wal under sustained decoy writes. SQLite's passive auto-checkpoint
	// (wal_autocheckpoint pages) can't reset the WAL past a held read-mark, and the
	// dashboard polls readers continuously, so it starves → the -wal grows unbounded
	// → every RO reader scans a multi-GB WAL (the 15-20s hang). A TRUNCATE checkpoint
	// on the flush loop resets it every dbFlushPeriod. It also reclaims the
	// DecoySource second-writer connection's frames (same file, it never checkpoints).
	// Run OUTSIDE d.lock: the single MaxOpenConns=1 writer conn already serializes it
	// against pruneOldest, so holding d.lock across its up-to-5s busy-wait only
	// needlessly pinned the writer mutex and starved retention. sqlite-only (aggregate).
	if d.aggregate {
		d.checkpointWAL()
	}

	return nil
}

// flushPending drains the pending-entries buffer under d.lock. Split out of
// doDBWrite so the WAL checkpoint runs after the lock is released.
func (d *DatabaseWriter) flushPending() error {
	d.lock.Lock()
	defer d.lock.Unlock()

	var err *multierror.Error

	if len(d.pendingEntries) > 0 {
		log.Log().Tracef("%d entries to write", len(d.pendingEntries))

		pending := len(d.pendingEntries)

		const bulkSize = 100

		var failed []*logEntry

		for i := 0; i < len(d.pendingEntries); i += bulkSize {
			j := min(i+bulkSize, len(d.pendingEntries))
			batch := d.pendingEntries[i:j]

			// raw rows and their aggregate deltas commit atomically: a crash can't
			// leave the dashboards (aggregates) out of sync with the raw log
			txErr := d.db.Transaction(func(tx *gorm.DB) error {
				if txErr := tx.Create(batch).Error; txErr != nil {
					return txErr
				}

				if d.aggregate {
					if aggErr := upsertAggregates(tx, batch); aggErr != nil {
						return aggErr
					}

					// persistent visited-domains corpus (T3) rides the same tx
					if ncErr := upsertNoiseCorpus(tx, batch); ncErr != nil {
						return ncErr
					}

					// durable fingerprint-keyed heuristics (identity/presence/usage/
					// class accumulators) fold into the SAME tx, so each delta commits
					// before any purge can delete the raw row it came from.
					return upsertHeuristics(tx, batch)
				}

				return nil
			})
			if txErr != nil {
				err = multierror.Append(err, txErr)
				// The whole tx (raw rows + aggregate + corpus deltas) rolled back
				// atomically, so keep this batch to retry next flush instead of
				// dropping it. Retrying re-applies all three without double-counting
				// the batches that DID commit.
				failed = append(failed, batch...)
			}
		}

		// LRU-cap the persistent noise corpus once per flush (sqlite-only). Best-effort
		// cap, not a data path — its failure never holds entries back for retry.
		if d.aggregate {
			err = multierror.Append(err, pruneNoiseCorpus(d.db))
			err = multierror.Append(err, pruneServiceUsage(d.db))
		}

		// Retain only the failed batches (silent query-log loss otherwise: the old
		// unconditional nil dropped the entire buffer on any SQLITE_BUSY under
		// write-lock contention), capped to the newest maxRetainedEntries: while
		// flushes fail persistently (read-only FS, SQLITE_FULL, corruption) an
		// unbounded retry buffer grows tens of MB/day until the OOM killer takes
		// household DNS down with it — drop the oldest instead.
		droppedOldest := 0
		if len(failed) > maxRetainedEntries {
			droppedOldest = len(failed) - maxRetainedEntries
			failed = failed[droppedOldest:]
		}

		d.pendingEntries = failed

		if multiErr := err.ErrorOrNil(); multiErr != nil {
			// WARN: some batches rolled back and are retained for retry. The audit
			// fixed the silent drop here — surface the loss-risk so it's visible.
			log.PrefixedLog("database_writer").
				WithField("written", pending-len(failed)-droppedOldest).
				WithField("retained_for_retry", len(failed)).
				WithField("dropped_oldest", droppedOldest).
				WithError(multiErr).
				Warn("query-log flush partially failed")

			return fmt.Errorf("failed to write querylog entries to database: %w", multiErr)
		}

		// Debug (hot path): flush outcome on success.
		log.PrefixedLog("database_writer").
			WithField("written", pending).
			Debug("query-log flush complete")
	}

	return nil
}

// checkpointWAL runs a TRUNCATE checkpoint on the writer connection and inspects
// its result. PRAGMA wal_checkpoint returns (busy, log, checkpointed): busy=1 means
// a reader/writer held the WAL so it could not reset it, and log>checkpointed means
// frames remain — either way the WAL was NOT fully truncated, the starvation this
// checkpoint exists to prevent. That was previously swallowed to Tracef; surface it
// at WARN with a running counter so a starving WAL is visible in the logs.
func (d *DatabaseWriter) checkpointWAL() {
	var res struct {
		Busy         int `gorm:"column:busy"`
		Log          int `gorm:"column:log"`
		Checkpointed int `gorm:"column:checkpointed"`
	}

	if err := d.db.Raw("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&res).Error; err != nil {
		log.Log().Tracef("wal checkpoint failed: %v", err)

		return
	}

	if res.Busy != 0 || res.Log != res.Checkpointed {
		n := d.checkpointBusy.Add(1)
		log.PrefixedLog("database_writer").
			WithField("busy", res.Busy).
			WithField("wal_frames", res.Log).
			WithField("checkpointed", res.Checkpointed).
			WithField("busy_total", n).
			Warn("WAL TRUNCATE checkpoint left frames (reader holds a mark) — WAL not fully reset")
	}
}
