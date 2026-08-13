package querylog

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/0xERR0R/blocky/log"
)

// Reader is a read-only handle on the sqlite query log for the UI stats API.
// All dashboard methods read the hourly aggregate tables (see aggregates.go);
// only Search touches the raw log_entries table (the query explorer needs the
// individual rows and is time-bounded + index-backed).
type Reader struct {
	db *gorm.DB
}

// NewReader opens the query-log database read-only (mode=ro, busy_timeout).
func NewReader(sqlitePath string) (*Reader, error) {
	dialector, err := newSQLiteReadOnlyDialector(sqlitePath)
	if err != nil {
		return nil, err
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: gormlogger.New(
			log.Log(),
			gormlogger.Config{
				SlowThreshold: time.Minute,
				LogLevel:      gormlogger.Warn,
				Colorful:      false,
			}),
	})
	if err != nil {
		return nil, fmt.Errorf("can't open query log database read-only: %w", err)
	}

	// mode=ro still defers the actual open; ping so a missing file fails here
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("can't access query log connection pool: %w", err)
	}

	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()

		return nil, fmt.Errorf("can't open query log database read-only: %w", err)
	}

	return &Reader{db: db}, nil
}

func (r *Reader) Close() error {
	sqlDB, err := r.db.DB()
	if err != nil {
		return err
	}

	return sqlDB.Close()
}

// latHistogram is the fixed latency histogram of aggHourly, summed over a time range.
type latHistogram [6]uint64

//nolint:gochecknoglobals // static bucket bounds of the fixed histogram
var latBounds = [6][2]float64{{0, 10}, {10, 50}, {50, 100}, {100, 250}, {250, 1000}, {1000, 1000}}

// percentile linearly interpolates within the bucket containing the p-th
// percentile. The open-ended last bucket reports its lower bound (1000ms).
func (h latHistogram) percentile(p float64) float64 {
	var total uint64
	for _, c := range h {
		total += c
	}

	if total == 0 {
		return 0
	}

	target := p / 100 * float64(total)

	var cum float64

	for i, c := range h {
		if cum+float64(c) >= target {
			lo, hi := latBounds[i][0], latBounds[i][1]
			if c == 0 {
				return lo
			}

			return lo + (target-cum)/float64(c)*(hi-lo)
		}

		cum += float64(c)
	}

	return latBounds[len(latBounds)-1][0]
}

type histRow struct {
	L0 uint64 `gorm:"column:l0"`
	L1 uint64 `gorm:"column:l1"`
	L2 uint64 `gorm:"column:l2"`
	L3 uint64 `gorm:"column:l3"`
	L4 uint64 `gorm:"column:l4"`
	L5 uint64 `gorm:"column:l5"`
}

func (h *histRow) histogram() latHistogram {
	return latHistogram{h.L0, h.L1, h.L2, h.L3, h.L4, h.L5}
}

const histSelect = `COALESCE(SUM(lat_0_10),0) AS l0, COALESCE(SUM(lat_10_50),0) AS l1,
COALESCE(SUM(lat_50_100),0) AS l2, COALESCE(SUM(lat_100_250),0) AS l3,
COALESCE(SUM(lat_250_1000),0) AS l4, COALESCE(SUM(lat_1000_inf),0) AS l5`

// Overview is the /api/ui/stats/overview response.
type Overview struct {
	Queries int64   `json:"queries"`
	Blocked int64   `json:"blocked"`
	Cached  int64   `json:"cached"`
	Clients int64   `json:"clients"`
	AvgMs   float64 `json:"avgMs"`
	P95Ms   float64 `json:"p95Ms"`
}

func (r *Reader) Overview(from, to time.Time) (*Overview, error) {
	var row struct {
		Hist    histRow `gorm:"embedded"`
		Queries int64   `gorm:"column:queries"`
		Blocked int64   `gorm:"column:blocked"`
		Cached  int64   `gorm:"column:cached"`
		Clients int64   `gorm:"column:clients"`
		SumDur  int64   `gorm:"column:sum_dur"`
	}

	err := r.db.Raw(`SELECT COALESCE(SUM(cnt),0) AS queries,
		COALESCE(SUM(CASE WHEN response_type = 'BLOCKED' THEN cnt ELSE 0 END),0) AS blocked,
		COALESCE(SUM(CASE WHEN response_type = 'CACHED' THEN cnt ELSE 0 END),0) AS cached,
		COUNT(DISTINCT client_name) AS clients,
		COALESCE(SUM(sum_duration_ms),0) AS sum_dur, `+histSelect+`
		FROM agg_hourly WHERE hour >= ? AND hour < ?`, from.UTC(), to.UTC()).Scan(&row).Error
	if err != nil {
		return nil, err
	}

	o := &Overview{
		Queries: row.Queries,
		Blocked: row.Blocked,
		Cached:  row.Cached,
		Clients: row.Clients,
		P95Ms:   row.Hist.histogram().percentile(95), //nolint:mnd
	}
	if row.Queries > 0 {
		o.AvgMs = float64(row.SumDur) / float64(row.Queries)
	}

	return o, nil
}

// Bucket is one time slot of the /api/ui/stats/buckets response.
type Bucket struct {
	TS     int64            `json:"ts"`
	Counts map[string]int64 `json:"counts"`
}

// Buckets returns per-response-type counts grouped into step-second slots.
// The finest stored granularity is one hour: sub-hour steps are clamped to
// 3600 (raw-row scans are forbidden for dashboards; the Live page derives
// sub-hour resolution client-side from the SSE stream).
func (r *Reader) Buckets(from, to time.Time, step int64) ([]Bucket, error) {
	const hourSeconds = 3600
	if step < hourSeconds {
		step = hourSeconds
	}

	var rows []struct {
		Hour         time.Time `gorm:"column:hour"`
		ResponseType string    `gorm:"column:response_type"`
		Cnt          int64     `gorm:"column:cnt"`
	}

	err := r.db.Raw(`SELECT hour, response_type, SUM(cnt) AS cnt FROM agg_hourly
		WHERE hour >= ? AND hour < ? GROUP BY hour, response_type`,
		from.UTC(), to.UTC()).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	byTS := map[int64]map[string]int64{}

	for _, row := range rows {
		ts := row.Hour.Unix() / step * step
		if byTS[ts] == nil {
			byTS[ts] = map[string]int64{}
		}

		byTS[ts][row.ResponseType] += row.Cnt
	}

	buckets := make([]Bucket, 0, len(byTS))
	for ts, counts := range byTS {
		buckets = append(buckets, Bucket{TS: ts, Counts: counts})
	}

	sort.Slice(buckets, func(i, j int) bool { return buckets[i].TS < buckets[j].TS })

	return buckets, nil
}

// TopItem is one entry of the /api/ui/stats/top response.
type TopItem struct {
	Name  string `gorm:"column:name"  json:"name"`
	Count int64  `gorm:"column:c"     json:"count"`
}

// Top returns the n most frequent values of col ("domain", "blocked",
// "client", "transport" or "fphash") in the time range. domain/blocked come
// from agg_domains_hourly; the rest from agg_hourly (fphash is part of that
// table's composite key, see aggregates.go).
func (r *Reader) Top(from, to time.Time, col string, n int) ([]TopItem, error) {
	var query string

	switch col {
	case "domain":
		query = `SELECT etldp AS name, SUM(cnt) AS c FROM agg_domains_hourly
			WHERE hour >= ? AND hour < ? GROUP BY etldp`
	case "blocked":
		query = `SELECT etldp AS name, SUM(cnt) AS c FROM agg_domains_hourly
			WHERE hour >= ? AND hour < ? AND blocked GROUP BY etldp`
	case "client":
		query = `SELECT client_name AS name, SUM(cnt) AS c FROM agg_hourly
			WHERE hour >= ? AND hour < ? GROUP BY client_name`
	case "transport":
		query = `SELECT transport AS name, SUM(cnt) AS c FROM agg_hourly
			WHERE hour >= ? AND hour < ? GROUP BY transport`
	case "fphash":
		query = `SELECT fp_hash AS name, SUM(cnt) AS c FROM agg_hourly
			WHERE hour >= ? AND hour < ? AND fp_hash <> '' GROUP BY fp_hash`
	default:
		return nil, fmt.Errorf("unknown top column: %q", col)
	}

	items := []TopItem{}
	err := r.db.Raw(query+" ORDER BY c DESC LIMIT ?", from.UTC(), to.UTC(), n).Scan(&items).Error

	return items, err
}

// Percentiles is the /api/ui/stats/latency response (milliseconds).
type Percentiles struct {
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

func (r *Reader) LatencyPercentiles(from, to time.Time) (*Percentiles, error) {
	var row histRow

	err := r.db.Raw(`SELECT `+histSelect+` FROM agg_hourly WHERE hour >= ? AND hour < ?`,
		from.UTC(), to.UTC()).Scan(&row).Error
	if err != nil {
		return nil, err
	}

	h := row.histogram()

	return &Percentiles{
		P50: h.percentile(50), //nolint:mnd
		P90: h.percentile(90), //nolint:mnd
		P95: h.percentile(95), //nolint:mnd
		P99: h.percentile(99), //nolint:mnd
	}, nil
}

const (
	searchDefaultLimit = 100
	searchMaxLimit     = 500
)

// SearchFilter narrows a raw query-log search. Zero From/To default to the
// last 24h; Limit defaults to 100 and is capped at 500.
type SearchFilter struct {
	Client        string
	Domain        string
	Qtype         string
	Rtype         string
	From          time.Time
	To            time.Time
	Limit         int
	Offset        int
	IncludeDecoys bool
}

// Search reads the raw log_entries table (explorer use case): time-bounded and
// served by the existing indexes. Domain filter: a value containing '*' is a
// wildcard pattern ('*' -> '%', so "ads.*" is a prefix LIKE); otherwise it is a
// substring match ('%value%'), both bounded by the time index.
func (r *Reader) Search(filter SearchFilter) (total int64, items []QueryItem, err error) {
	to := filter.To
	if to.IsZero() {
		to = time.Now()
	}

	from := filter.From
	if from.IsZero() {
		from = to.Add(-24 * time.Hour) //nolint:mnd
	}

	q := r.db.Model(&logEntry{}).Where("request_ts >= ? AND request_ts <= ?", from, to)

	if !filter.IncludeDecoys {
		q = q.Where("decoy = ?", false)
	}

	if filter.Client != "" {
		q = q.Where("(client_ip = ? OR client_name LIKE ?)", filter.Client, "%"+filter.Client+"%")
	}

	if filter.Domain != "" {
		pattern := "%" + filter.Domain + "%"
		if strings.Contains(filter.Domain, "*") {
			pattern = strings.ReplaceAll(filter.Domain, "*", "%")
		}

		q = q.Where("question_name LIKE ?", pattern)
	}

	if filter.Qtype != "" {
		q = q.Where("question_type = ?", filter.Qtype)
	}

	if filter.Rtype != "" {
		q = q.Where("response_type = ?", filter.Rtype)
	}

	// reusable session: Count and Find below share the same WHERE clause
	q = q.Session(&gorm.Session{})

	if err := q.Count(&total).Error; err != nil {
		return 0, nil, err
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = searchDefaultLimit
	}

	limit = min(limit, searchMaxLimit)

	var rows []logEntry
	if err := q.Order("request_ts DESC").Limit(limit).Offset(filter.Offset).Find(&rows).Error; err != nil {
		return 0, nil, err
	}

	items = make([]QueryItem, 0, len(rows))
	for i := range rows {
		items = append(items, itemFromRow(&rows[i]))
	}

	return total, items, nil
}

func itemFromRow(e *logEntry) QueryItem {
	var clientNames []string
	if e.ClientName != "" {
		clientNames = strings.Split(e.ClientName, "; ")
	}

	return QueryItem{
		TS:          e.RequestTS.Format(time.RFC3339),
		Client:      e.ClientIP,
		ClientNames: clientNames,
		Question:    e.QuestionName,
		Qtype:       e.QuestionType,
		Rtype:       e.ResponseType,
		Rcode:       e.ResponseCode,
		Answer:      e.Answer,
		DurationMs:  e.DurationMs,
		Transport:   e.Transport,
		FpHash:      e.FpHash,
		Reason:      e.Reason,
		Decoy:       e.Decoy,
	}
}

// TotalQueries counts all raw log rows (decoys included), for /api/ui/system.
func (r *Reader) TotalQueries() (int64, error) {
	var count int64

	return count, r.db.Model(&logEntry{}).Count(&count).Error
}
