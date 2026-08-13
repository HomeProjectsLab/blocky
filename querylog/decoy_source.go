package querylog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/0xERR0R/blocky/log"
)

// decoyReplayWindow bounds the replay pool to recent real queries: sampling
// from a rolling window keeps replays plausible (an observer subtracting an
// old, no-longer-visited domain would stand out) and keeps ORDER BY RANDOM()
// cheap (the request_ts index bounds the scan).
const decoyReplayWindow = 7 * 24 * time.Hour

// decoySeedBatch is the insert batch size used when seeding decoy_domains, so a
// 1M-row list never materializes as a single in-RAM slice or one giant INSERT.
const decoySeedBatch = 2000

// DecoySource is a read-write handle on the query-log sqlite database used by
// the decoy engine. It co-locates two noise sources on one connection: the
// static Tranco list (decoy_domains, seeded once) sampled by random rowid, and
// the real-query replay pool (recent non-decoy log_entries rows).
type DecoySource struct {
	db *gorm.DB

	mu         sync.Mutex
	rnd        *rand.Rand
	maxRowid   int64 // cached after seeding; decoy_domains is insert-only read-only
	blMaxRowid int64 // cached max rowid of blocklist_domains for random-rowid sampling
}

// decoyDomain is the gorm model for the seeded Tranco list. rowid is SQLite's
// implicit primary key (INTEGER PRIMARY KEY aliases it); inserts are contiguous
// so random-rowid sampling has no gaps.
type decoyDomain struct {
	Rowid  int64  `gorm:"column:rowid;primaryKey"`
	Domain string `gorm:"column:domain;not null"`
}

func (decoyDomain) TableName() string { return "decoy_domains" }

// RealQuery is one sampled row from the replay pool.
type RealQuery struct {
	Name  string `gorm:"column:question_name"`
	Qtype string `gorm:"column:question_type"`
}

// NewDecoySource opens the query-log database read-write (same WAL DSN as the
// writer, so both connections share the file). The writer creates the file and
// the log_entries table before this is called.
func NewDecoySource(sqlitePath string) (*DecoySource, error) {
	dialector, err := newSQLiteDialector(sqlitePath)
	if err != nil {
		return nil, err
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: gormlogger.New(log.Log(), gormlogger.Config{
			SlowThreshold: time.Minute,
			LogLevel:      gormlogger.Warn,
			Colorful:      false,
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("can't open query log database for decoy source: %w", err)
	}

	if err := db.AutoMigrate(&decoyDomain{}, &blocklistDomain{}, &listMeta{}); err != nil {
		return nil, fmt.Errorf("can't create list tables: %w", err)
	}

	s := &DecoySource{db: db, rnd: rand.New(rand.NewSource(time.Now().UnixNano()))} //nolint:gosec // noise timing, not crypto

	if err := s.loadMaxRowid(); err != nil {
		return nil, err
	}

	if err := s.loadBlMaxRowid(); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *DecoySource) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}

	return sqlDB.Close()
}

func (s *DecoySource) loadMaxRowid() error {
	return s.db.Raw("SELECT COALESCE(MAX(rowid),0) FROM decoy_domains").Scan(&s.maxRowid).Error
}

// SeedIfEmpty streams normalized domains (one per line, already decompressed)
// into decoy_domains, but only when the table is empty. Blank lines are
// skipped. Returns the number of rows inserted (0 when already seeded).
func (s *DecoySource) SeedIfEmpty(r io.Reader) (int, error) {
	if s.maxRowid > 0 {
		return 0, nil
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) //nolint:mnd // allow long lines

	batch := make([]decoyDomain, 0, decoySeedBatch)
	inserted := 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}

		if err := s.db.Create(&batch).Error; err != nil {
			return fmt.Errorf("can't seed decoy_domains: %w", err)
		}

		inserted += len(batch)
		batch = batch[:0]

		return nil
	}

	for scanner.Scan() {
		domain := scanner.Text()
		if domain == "" {
			continue
		}

		batch = append(batch, decoyDomain{Domain: domain})
		if len(batch) >= decoySeedBatch {
			if err := flush(); err != nil {
				return inserted, err
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return inserted, fmt.Errorf("can't read decoy list: %w", err)
	}

	if err := flush(); err != nil {
		return inserted, err
	}

	return inserted, s.loadMaxRowid()
}

// SampleList returns a random domain from the static list by picking a random
// rowid and taking the first row at or after it (gap-tolerant). Empty string
// when the list is not seeded.
func (s *DecoySource) SampleList() (string, error) {
	s.mu.Lock()
	max := s.maxRowid
	k := int64(0)
	if max > 0 {
		k = s.rnd.Int63n(max) + 1
	}
	s.mu.Unlock()

	if max == 0 {
		return "", nil
	}

	var domain string
	err := s.db.Raw("SELECT domain FROM decoy_domains WHERE rowid >= ? ORDER BY rowid LIMIT 1", k).Scan(&domain).Error

	return domain, err
}

// HourlyRealCounts returns real (non-decoy) query counts bucketed by UTC
// hour-of-day over the recent window, read from the agg_hourly aggregate table
// (which already excludes decoy rows). Used to shape the decoy rate to real
// activity. A missing agg_hourly table (non-sqlite target, or before the writer
// creates it) surfaces as an error and the caller treats it as cold start.
func (s *DecoySource) HourlyRealCounts() ([24]int64, error) {
	var counts [24]int64

	since := time.Now().Add(-decoyReplayWindow)

	var rows []struct {
		Hour time.Time `gorm:"column:hour"`
		Cnt  int64     `gorm:"column:cnt"`
	}

	if err := s.db.Raw(`SELECT hour, cnt FROM agg_hourly WHERE hour >= ?`, since.UTC()).Scan(&rows).Error; err != nil {
		return counts, err
	}

	for _, r := range rows {
		counts[r.Hour.UTC().Hour()] += r.Cnt
	}

	return counts, nil
}

// FpSample is a transport-agnostic EDNS/qtype shape sampled from a real client.
type FpSample struct {
	Qtype       string // question type of the sampled real query
	HadEDNS0    bool   // whether the real query carried an OPT record
	EDNSUDPSize uint16 // advertised UDP payload size
	DO          bool   // DNSSEC OK bit
}

// SampleRealFingerprint returns the EDNS shape of one random recent real query
// so decoys can be stamped to match the real client-software distribution
// (otherwise every synthetic query looks like the resolver's default). Zero
// value (HadEDNS0 false) at cold start — caller then leaves the plain query.
func (s *DecoySource) SampleRealFingerprint() (FpSample, error) {
	since := time.Now().Add(-decoyReplayWindow)

	var row struct {
		QuestionType string `gorm:"column:question_type"`
		EDNSUDPSize  uint16 `gorm:"column:edns_udp_size"`
		FpDetail     string `gorm:"column:fp_detail"`
	}

	err := s.db.Raw(`SELECT question_type, edns_udp_size, fp_detail FROM log_entries
		WHERE decoy = 0 AND question_name <> '' AND request_ts >= ?
		ORDER BY RANDOM() LIMIT 1`, since).Scan(&row).Error
	if err != nil {
		return FpSample{}, err
	}

	fp := FpSample{Qtype: row.QuestionType, EDNSUDPSize: row.EDNSUDPSize}

	// DO and HadEDNS0 live inside the fp_detail JSON blob, not in columns.
	if row.FpDetail != "" {
		var d struct {
			DO       bool `json:"do"`
			HadEDNS0 bool `json:"hadEdns0"`
		}

		if json.Unmarshal([]byte(row.FpDetail), &d) == nil {
			fp.DO = d.DO
			fp.HadEDNS0 = d.HadEDNS0
		}
	}

	return fp, nil
}

// SampleRecentReal returns up to limit real (non-decoy) queries sampled at
// random from the recent window. Empty slice at cold start (no history yet).
func (s *DecoySource) SampleRecentReal(limit int) ([]RealQuery, error) {
	since := time.Now().Add(-decoyReplayWindow)

	var out []RealQuery
	err := s.db.Raw(`SELECT question_name, question_type FROM log_entries
		WHERE decoy = 0 AND question_name <> '' AND request_ts >= ?
		ORDER BY RANDOM() LIMIT ?`, since, limit).Scan(&out).Error

	return out, err
}
