package querylog

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Write-path aggregates: dashboards read these tables, never the raw log rows.
// Both tables are maintained in the same transaction as each raw batch insert
// (see doDBWrite) and are sqlite-only, like the /api/ui stats reader.
// Decoy entries are excluded here (they stay in the raw table only).

// aggHourly is one hour of query counts per client/response-type/transport/fp-hash.
// FpHash lives in this composite key (instead of a third aggregate table): it is a
// per-client-software hash, so its cardinality per client stays small, and it lets
// Top(fphash) run off the same table as client/transport.
type aggHourly struct {
	Hour          time.Time `gorm:"column:hour;primaryKey"`
	ClientName    string    `gorm:"column:client_name;primaryKey"`
	ResponseType  string    `gorm:"column:response_type;primaryKey"`
	Transport     string    `gorm:"column:transport;primaryKey"`
	FpHash        string    `gorm:"column:fp_hash;primaryKey"`
	Cnt           uint64    `gorm:"column:cnt"`
	SumDurationMs uint64    `gorm:"column:sum_duration_ms"`
	// fixed latency histogram (milliseconds), bounds: [0,10) [10,50) [50,100) [100,250) [250,1000) [1000,inf)
	Lat0_10     uint64 `gorm:"column:lat_0_10"`
	Lat10_50    uint64 `gorm:"column:lat_10_50"`
	Lat50_100   uint64 `gorm:"column:lat_50_100"`
	Lat100_250  uint64 `gorm:"column:lat_100_250"`
	Lat250_1000 uint64 `gorm:"column:lat_250_1000"`
	Lat1000Inf  uint64 `gorm:"column:lat_1000_inf"`
}

func (aggHourly) TableName() string { return "agg_hourly" }

// aggDomainHourly is one hour of query counts per effective TLD+1.
type aggDomainHourly struct {
	Hour    time.Time `gorm:"column:hour;primaryKey"`
	Etldp   string    `gorm:"column:etldp;primaryKey"`
	Blocked bool      `gorm:"column:blocked;primaryKey"`
	Cnt     uint64    `gorm:"column:cnt"`
}

func (aggDomainHourly) TableName() string { return "agg_domains_hourly" }

func (a *aggHourly) addLatency(ms int64) {
	switch {
	case ms < 10:
		a.Lat0_10++
	case ms < 50:
		a.Lat10_50++
	case ms < 100:
		a.Lat50_100++
	case ms < 250:
		a.Lat100_250++
	case ms < 1000:
		a.Lat250_1000++
	default:
		a.Lat1000Inf++
	}
}

type aggHourlyKey struct {
	hour                                    int64
	client, responseType, transport, fpHash string
}

type aggDomainKey struct {
	hour    int64
	etldp   string
	blocked bool
}

// upsertAggregates folds a raw batch into the hourly aggregate tables inside tx.
// Rows are deduplicated per key in memory first, so a single multi-row upsert per
// table suffices and the ON CONFLICT expressions never see the same key twice.
func upsertAggregates(tx *gorm.DB, entries []*logEntry) error {
	hourly := map[aggHourlyKey]*aggHourly{}
	domains := map[aggDomainKey]*aggDomainHourly{}

	for _, e := range entries {
		if e.Decoy {
			continue
		}

		hour := e.RequestTS.Truncate(time.Hour).UTC()

		hk := aggHourlyKey{hour.Unix(), e.ClientName, e.ResponseType, e.Transport, e.FpHash}
		h := hourly[hk]

		if h == nil {
			h = &aggHourly{
				Hour: hour, ClientName: e.ClientName, ResponseType: e.ResponseType,
				Transport: e.Transport, FpHash: e.FpHash,
			}
			hourly[hk] = h
		}

		h.Cnt++
		h.SumDurationMs += uint64(max(e.DurationMs, 0))
		h.addLatency(e.DurationMs)

		etldp := e.EffectiveTLDP
		if etldp == "" {
			etldp = e.QuestionName
		}

		blocked := e.ResponseType == "BLOCKED"

		dk := aggDomainKey{hour.Unix(), etldp, blocked}
		d := domains[dk]

		if d == nil {
			d = &aggDomainHourly{Hour: hour, Etldp: etldp, Blocked: blocked}
			domains[dk] = d
		}

		d.Cnt++
	}

	if len(hourly) > 0 {
		rows := make([]*aggHourly, 0, len(hourly))
		for _, h := range hourly {
			rows = append(rows, h)
		}

		err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "hour"},
				{Name: "client_name"},
				{Name: "response_type"},
				{Name: "transport"},
				{Name: "fp_hash"},
			},
			DoUpdates: clause.Assignments(map[string]any{
				"cnt":             gorm.Expr("cnt + excluded.cnt"),
				"sum_duration_ms": gorm.Expr("sum_duration_ms + excluded.sum_duration_ms"),
				"lat_0_10":        gorm.Expr("lat_0_10 + excluded.lat_0_10"),
				"lat_10_50":       gorm.Expr("lat_10_50 + excluded.lat_10_50"),
				"lat_50_100":      gorm.Expr("lat_50_100 + excluded.lat_50_100"),
				"lat_100_250":     gorm.Expr("lat_100_250 + excluded.lat_100_250"),
				"lat_250_1000":    gorm.Expr("lat_250_1000 + excluded.lat_250_1000"),
				"lat_1000_inf":    gorm.Expr("lat_1000_inf + excluded.lat_1000_inf"),
			}),
		}).Create(&rows).Error
		if err != nil {
			return err
		}
	}

	if len(domains) > 0 {
		rows := make([]*aggDomainHourly, 0, len(domains))
		for _, d := range domains {
			rows = append(rows, d)
		}

		err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "hour"}, {Name: "etldp"}, {Name: "blocked"}},
			DoUpdates: clause.Assignments(map[string]any{"cnt": gorm.Expr("cnt + excluded.cnt")}),
		}).Create(&rows).Error
		if err != nil {
			return err
		}
	}

	return nil
}
