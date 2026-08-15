package querylog

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
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
	RequestTS     time.Time `gorm:"not null;index;index:idx_client_name_request_ts,priority:2"`
	ClientIP      string
	ClientName    string `gorm:"index;index:idx_client_name_request_ts,priority:1"`
	DurationMs    int64
	Reason        string
	ResponseType  string `gorm:"index"`
	QuestionType  string
	QuestionName  string `gorm:"index"`
	EffectiveTLDP string
	Answer        string
	ResponseCode  string
	Hostname      string
	Transport     string `gorm:"index"`
	FpHash        string `gorm:"index"`
	DoHUserAgent  string `gorm:"column:doh_user_agent"`
	SNI           string `gorm:"column:sni"`
	EDNSUDPSize   uint16 `gorm:"column:edns_udp_size"`
	EDNSOptCodes  string `gorm:"column:edns_opt_codes"` // wire order, e.g. "10,8,12"
	Decoy         bool   `gorm:"index"`
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

		// Enable incremental auto-vacuum so the disk guardian can return freed pages
		// to the OS via incremental_vacuum. SQLite silently ignores the pragma once
		// the file header is written — and the WAL DSN writes it on open — so the
		// pragma alone is a no-op (the db stays auto_vacuum=NONE even when fresh). A
		// single VACUUM right after the pragma actually applies the new mode, on both
		// a fresh WAL db and a pre-existing appliance db. Done once at startup (VACUUM
		// needs ~2x space) before the guardian runs, never under disk pressure.
		db.Exec("PRAGMA auto_vacuum = INCREMENTAL")
		db.Exec("VACUUM")
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

	return w, nil
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
		// API and the persistent visited-domains noise corpus (decoy source, T3).
		if err := db.AutoMigrate(&aggHourly{}, &aggDomainHourly{}, &noiseCorpus{}); err != nil {
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
		RequestTS:     entry.Start,
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

	log.PrefixedLog("database_writer").Debugf("deleting log entries with request_ts < %s", deletionDate)
	d.db.Where("request_ts < ?", deletionDate).Delete(&logEntry{})
}

func (d *DatabaseWriter) doDBWrite() error {
	d.lock.Lock()
	defer d.lock.Unlock()

	var err *multierror.Error

	if len(d.pendingEntries) > 0 {
		log.Log().Tracef("%d entries to write", len(d.pendingEntries))

		const bulkSize = 100

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
					return upsertNoiseCorpus(tx, batch)
				}

				return nil
			})
			err = multierror.Append(err, txErr)
		}

		// LRU-cap the persistent noise corpus once per flush (sqlite-only).
		if d.aggregate {
			err = multierror.Append(err, pruneNoiseCorpus(d.db))
		}

		// clear the slice with pending entries
		d.pendingEntries = nil

		if multiErr := err.ErrorOrNil(); multiErr != nil {
			return fmt.Errorf("failed to write querylog entries to database: %w", multiErr)
		}
	}

	return nil
}
