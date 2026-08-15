package querylog

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
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

	// TotalQueries is polled ~once/second by the dashboard and is a COUNT(*) over
	// the whole (unboundedly growing) log_entries table, so it is cached with a
	// short TTL — a display total doesn't need per-second freshness, and the raw
	// scan otherwise thrashes the disk on a large DB.
	totalMu    sync.Mutex
	totalCache int64
	totalAt    time.Time
}

// totalQueriesTTL bounds how often TotalQueries re-runs its COUNT(*).
const totalQueriesTTL = 30 * time.Second

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
	Name  string `gorm:"column:name" json:"name"`
	Count int64  `gorm:"column:c"    json:"count"`
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
		from = to.Add(-24 * time.Hour)
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
		DecoySource: e.DecoySource,
	}
}

// TotalQueries counts all raw log rows (decoys included), for /api/ui/system.
func (r *Reader) TotalQueries() (int64, error) {
	r.totalMu.Lock()
	defer r.totalMu.Unlock()

	if !r.totalAt.IsZero() && time.Since(r.totalAt) < totalQueriesTTL {
		return r.totalCache, nil
	}

	var count int64
	if err := r.db.Model(&logEntry{}).Count(&count).Error; err != nil {
		return r.totalCache, err // serve the last good value on a transient error
	}

	r.totalCache, r.totalAt = count, time.Now()

	return count, nil
}

// ClientRow is one entry of the /api/ui/clients list response. The enrichment
// fields (ips / natAggregate / fpCount / os / vendor / model / apps / legacy
// deviceGuess) are derived from the raw log_entries rows (see client_enrich.go);
// they're optional so an unenriched row stays lean. A NAT/shared identity has
// its facets blanked and carries shared/sharedLabel instead (blueprint R3).
type ClientRow struct {
	Name     string `json:"name"`
	Queries  int64  `json:"queries"`
	Blocked  int64  `json:"blocked"`
	LastSeen string `json:"lastSeen"`
	// DNS-native client identity enrichment.
	IPs          []string `json:"ips,omitempty"`
	NatAggregate bool     `json:"natAggregate,omitempty"`
	FpCount      int      `json:"fpCount,omitempty"`
	OS           string   `json:"os,omitempty"`
	Vendor       []string `json:"vendor,omitempty"`
	Model        []string `json:"model,omitempty"`
	Apps         []string `json:"apps,omitempty"`
	DeviceGuess  string   `json:"deviceGuess,omitempty"`
	Shared       bool     `json:"shared,omitempty"`
	SharedLabel  string   `json:"sharedLabel,omitempty"`
}

// applyEnrich copies the derived identity fields onto a ClientRow.
func (row *ClientRow) applyEnrich(e clientEnrich) {
	row.IPs, row.NatAggregate, row.FpCount = e.IPs, e.NatAggregate, e.FpCount
	row.OS, row.Vendor, row.Model, row.Apps = e.OS, e.Vendor, e.Model, e.Apps
	row.DeviceGuess, row.Shared, row.SharedLabel = e.DeviceGuess, e.Shared, e.SharedLabel
}

// ClientList ranks clients by query count in the range (from the hourly aggregates).
func (r *Reader) ClientList(from, to time.Time) ([]ClientRow, error) {
	var rows []struct {
		Name     string `gorm:"column:name"`
		Queries  int64  `gorm:"column:queries"`
		Blocked  int64  `gorm:"column:blocked"`
		LastHour string `gorm:"column:last_hour"` // MAX() drops sqlite datetime affinity -> text
	}

	err := r.db.Raw(`SELECT client_name AS name, SUM(cnt) AS queries,
		COALESCE(SUM(CASE WHEN response_type = 'BLOCKED' THEN cnt ELSE 0 END),0) AS blocked,
		MAX(hour) AS last_hour
		FROM agg_hourly WHERE hour >= ? AND hour < ? GROUP BY client_name ORDER BY queries DESC`,
		from.UTC(), to.UTC()).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	// one extra windowed scan of log_entries yields IPs / fp count / device guess
	// per client (the aggregates lack client_ip and a distinct-fp count).
	enrich, err := r.enrichClients(from, to)
	if err != nil {
		return nil, err
	}

	out := make([]ClientRow, 0, len(rows))
	for i := range rows {
		row := ClientRow{
			Name: rows[i].Name, Queries: rows[i].Queries, Blocked: rows[i].Blocked,
			LastSeen: normalizeSQLiteTime(rows[i].LastHour),
		}
		if e, ok := enrich[rows[i].Name]; ok {
			row.applyEnrich(e)
		}

		out = append(out, row)
	}

	return out, nil
}

// normalizeSQLiteTime reparses a sqlite datetime text (whose column affinity was
// lost by an aggregate like MAX()) into RFC3339; returns the input unchanged if
// no known layout matches.
func normalizeSQLiteTime(s string) string {
	if s == "" {
		return ""
	}

	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}

	return s
}

// FpCluster is one client-software fingerprint cluster with a representative
// sample of its EDNS features (pulled from the most recent matching raw row).
type FpCluster struct {
	FpHash       string `json:"fpHash"`
	Count        int64  `json:"count"`
	Transport    string `json:"transport"`
	DO           bool   `json:"do"`
	HadEDNS0     bool   `json:"hadEdns0"`
	HasCookie    bool   `json:"hasCookie"`
	EDNSUDPSize  uint16 `json:"ednsUdpSize"`
	EDNSOptCodes string `json:"ednsOptCodes"`
	DoHUserAgent string `json:"dohUserAgent"`
}

// ClientDetail is the /api/ui/clients/{name} response: activity history plus the
// fingerprint panel (transport mix, fp clusters, top domains).
type ClientDetail struct {
	Name         string      `json:"name"`
	Queries      int64       `json:"queries"`
	Blocked      int64       `json:"blocked"`
	History      []Bucket    `json:"history"`
	Transports   []TopItem   `json:"transports"`
	Fingerprints []FpCluster `json:"fingerprints"`
	TopDomains   []TopItem   `json:"topDomains"`
	// DNS-native client identity enrichment (see client_enrich.go).
	IPs          []string `json:"ips,omitempty"`
	NatAggregate bool     `json:"natAggregate,omitempty"`
	FpCount      int      `json:"fpCount,omitempty"`
	OS           string   `json:"os,omitempty"`
	Vendor       []string `json:"vendor,omitempty"`
	Model        []string `json:"model,omitempty"`
	Apps         []string `json:"apps,omitempty"`
	DeviceGuess  string   `json:"deviceGuess,omitempty"`
	Shared       bool     `json:"shared,omitempty"`
	SharedLabel  string   `json:"sharedLabel,omitempty"`
}

// ClientDetail assembles the drill-down for one client (keyed by the exact
// aggregated client_name). History/transports/fingerprints come from the hourly
// aggregates; top domains from the raw log (client-scoped, index-backed).
func (r *Reader) ClientDetail(name string, from, to time.Time) (*ClientDetail, error) {
	d := &ClientDetail{Name: name, History: []Bucket{}, Transports: []TopItem{}, Fingerprints: []FpCluster{}, TopDomains: []TopItem{}}

	var totals struct {
		Queries int64 `gorm:"column:queries"`
		Blocked int64 `gorm:"column:blocked"`
	}

	err := r.db.Raw(`SELECT COALESCE(SUM(cnt),0) AS queries,
		COALESCE(SUM(CASE WHEN response_type = 'BLOCKED' THEN cnt ELSE 0 END),0) AS blocked
		FROM agg_hourly WHERE client_name = ? AND hour >= ? AND hour < ?`,
		name, from.UTC(), to.UTC()).Scan(&totals).Error
	if err != nil {
		return nil, err
	}

	d.Queries, d.Blocked = totals.Queries, totals.Blocked

	var histRows []struct {
		Hour time.Time `gorm:"column:hour"`
		Cnt  int64     `gorm:"column:cnt"`
	}

	err = r.db.Raw(`SELECT hour, SUM(cnt) AS cnt FROM agg_hourly
		WHERE client_name = ? AND hour >= ? AND hour < ? GROUP BY hour ORDER BY hour`,
		name, from.UTC(), to.UTC()).Scan(&histRows).Error
	if err != nil {
		return nil, err
	}

	for _, h := range histRows {
		d.History = append(d.History, Bucket{TS: h.Hour.Unix(), Counts: map[string]int64{"queries": h.Cnt}})
	}

	err = r.db.Raw(`SELECT transport AS name, SUM(cnt) AS c FROM agg_hourly
		WHERE client_name = ? AND hour >= ? AND hour < ? GROUP BY transport ORDER BY c DESC`,
		name, from.UTC(), to.UTC()).Scan(&d.Transports).Error
	if err != nil {
		return nil, err
	}

	err = r.db.Raw(`SELECT COALESCE(NULLIF(effective_tldp,''), question_name) AS name, COUNT(*) AS c
		FROM log_entries WHERE client_name = ? AND request_ts >= ? AND request_ts <= ? AND decoy = 0
		GROUP BY name ORDER BY c DESC LIMIT 10`,
		name, from, to).Scan(&d.TopDomains).Error
	if err != nil {
		return nil, err
	}

	if err := r.fingerprintClusters(d, name, from, to); err != nil {
		return nil, err
	}

	// derive IPs / distinct-fp count / device guess from the raw rows
	e, err := r.enrichClient(name, from, to)
	if err != nil {
		return nil, err
	}

	d.IPs, d.NatAggregate, d.FpCount = e.IPs, e.NatAggregate, e.FpCount
	d.OS, d.Vendor, d.Model, d.Apps = e.OS, e.Vendor, e.Model, e.Apps
	d.DeviceGuess, d.Shared, d.SharedLabel = e.DeviceGuess, e.Shared, e.SharedLabel

	return d, nil
}

// Decoy stats read the raw log_entries table scoped to decoy = 1 (decoys are
// deliberately kept out of the hourly aggregates that back the real-traffic
// dashboards, so there's no aggregate table to read). They feed the Noise
// dashboard and mirror the real-traffic readers above, filtered to decoys.

// DecoyOverview is the /api/ui/stats/decoy/overview response.
type DecoyOverview struct {
	Decoys          int64            `json:"decoys"`          // total decoy queries in range
	DistinctDomains int64            `json:"distinctDomains"` // distinct fake domains (eTLD+1)
	BySource        map[string]int64 `json:"bySource"`        // provenance -> count
}

func (r *Reader) DecoyOverview(from, to time.Time) (*DecoyOverview, error) {
	var totals struct {
		Decoys          int64 `gorm:"column:decoys"`
		DistinctDomains int64 `gorm:"column:distinct_domains"`
	}

	err := r.db.Raw(`SELECT COUNT(*) AS decoys,
		COUNT(DISTINCT COALESCE(NULLIF(effective_tldp,''), question_name)) AS distinct_domains
		FROM log_entries WHERE decoy = 1 AND request_ts >= ? AND request_ts <= ?`,
		from, to).Scan(&totals).Error
	if err != nil {
		return nil, err
	}

	mix, err := r.DecoySourceMix(from, to)
	if err != nil {
		return nil, err
	}

	bySource := make(map[string]int64, len(mix))
	for _, m := range mix {
		bySource[m.Name] = m.Count
	}

	return &DecoyOverview{Decoys: totals.Decoys, DistinctDomains: totals.DistinctDomains, BySource: bySource}, nil
}

// DecoySourceMix returns the count per decoy_source (the replay:corpus:list:
// companion:cohort:chatter:miss:fail ratio) in the range, most frequent first.
// TopItem.Name is the provenance label.
func (r *Reader) DecoySourceMix(from, to time.Time) ([]TopItem, error) {
	items := []TopItem{}
	err := r.db.Raw(`SELECT decoy_source AS name, COUNT(*) AS c FROM log_entries
		WHERE decoy = 1 AND request_ts >= ? AND request_ts <= ?
		GROUP BY decoy_source ORDER BY c DESC`, from, to).Scan(&items).Error

	return items, err
}

// DecoyTopDomains returns the n most frequent fake domains (eTLD+1, falling back
// to the question name) emitted as decoys in the range.
func (r *Reader) DecoyTopDomains(from, to time.Time, n int) ([]TopItem, error) {
	items := []TopItem{}
	err := r.db.Raw(`SELECT COALESCE(NULLIF(effective_tldp,''), question_name) AS name, COUNT(*) AS c
		FROM log_entries WHERE decoy = 1 AND request_ts >= ? AND request_ts <= ?
		GROUP BY name ORDER BY c DESC LIMIT ?`, from, to, n).Scan(&items).Error

	return items, err
}

// DecoyBuckets returns decoy counts grouped into step-second slots, split by
// decoy_source (Bucket.Counts is source -> count). Unlike the real-traffic
// Buckets it reads raw rows, so any step is honoured (decoys are low-volume and
// time-bounded); a non-positive step falls back to one hour.
func (r *Reader) DecoyBuckets(from, to time.Time, step int64) ([]Bucket, error) {
	if step <= 0 {
		step = 3600
	}

	var rows []struct {
		TS     time.Time `gorm:"column:request_ts"`
		Source string    `gorm:"column:decoy_source"`
	}

	err := r.db.Raw(`SELECT request_ts, decoy_source FROM log_entries
		WHERE decoy = 1 AND request_ts >= ? AND request_ts <= ?`, from, to).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	byTS := map[int64]map[string]int64{}

	for _, row := range rows {
		ts := row.TS.Unix() / step * step
		if byTS[ts] == nil {
			byTS[ts] = map[string]int64{}
		}

		byTS[ts][row.Source]++
	}

	buckets := make([]Bucket, 0, len(byTS))
	for ts, counts := range byTS {
		buckets = append(buckets, Bucket{TS: ts, Counts: counts})
	}

	sort.Slice(buckets, func(i, j int) bool { return buckets[i].TS < buckets[j].TS })

	return buckets, nil
}

func (r *Reader) fingerprintClusters(d *ClientDetail, name string, from, to time.Time) error {
	var fpRows []TopItem

	err := r.db.Raw(`SELECT fp_hash AS name, SUM(cnt) AS c FROM agg_hourly
		WHERE client_name = ? AND hour >= ? AND hour < ? AND fp_hash <> '' GROUP BY fp_hash ORDER BY c DESC`,
		name, from.UTC(), to.UTC()).Scan(&fpRows).Error
	if err != nil {
		return err
	}

	for _, fp := range fpRows {
		cluster := FpCluster{FpHash: fp.Name, Count: fp.Count}

		var sample struct {
			Transport    string `gorm:"column:transport"`
			EDNSUDPSize  uint16 `gorm:"column:edns_udp_size"`
			EDNSOptCodes string `gorm:"column:edns_opt_codes"`
			DoHUserAgent string `gorm:"column:doh_user_agent"`
			FpDetail     string `gorm:"column:fp_detail"`
		}

		// most recent raw row for this cluster carries the EDNS feature sample
		err := r.db.Raw(`SELECT transport, edns_udp_size, edns_opt_codes, doh_user_agent, fp_detail
			FROM log_entries WHERE client_name = ? AND fp_hash = ? ORDER BY request_ts DESC LIMIT 1`,
			name, fp.Name).Scan(&sample).Error
		if err != nil {
			return err
		}

		cluster.Transport = sample.Transport
		cluster.EDNSUDPSize = sample.EDNSUDPSize
		cluster.EDNSOptCodes = sample.EDNSOptCodes
		cluster.DoHUserAgent = sample.DoHUserAgent

		if sample.FpDetail != "" {
			var det fpDetail
			if json.Unmarshal([]byte(sample.FpDetail), &det) == nil {
				cluster.DO = det.DO
				cluster.HadEDNS0 = det.HadEDNS0
				cluster.HasCookie = det.HasCookie
			}
		}

		d.Fingerprints = append(d.Fingerprints, cluster)
	}

	return nil
}
