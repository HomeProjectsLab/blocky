package querylog

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Client identity enrichment derived entirely from the already-persisted raw
// log_entries rows (client_ip + fp_hash + question_name). No schema change, no
// migration: the aggregate tables that back ClientList/ClientDetail dropped
// client_ip and can't count distinct fingerprints, so this reads log_entries
// directly. Client lists are small, so one extra windowed SELECT is cheap.
//
// Recognition is multi-facet: a single-valued OS (conf-ranked, MAX wins) plus
// multi-valued vendor / model / app sets (GROUP_CONCAT, med+ confidence only).
// All of it is a read-time expansion of the same GROUP BY client_name scan —
// no window function, no new scan shape (blueprint R1).

// natAggregateFpThreshold: a client (one client_name/IP) showing at least this
// many DISTINCT fingerprint hashes is almost certainly many devices behind one
// identity (a router/hairpin, e.g. 10.128.48.1), so we flag it — and suppress
// its facets, since a union-of-all-vendors is meaningless (blueprint R3).
//
// ponytail: fixed distinct-fp threshold; make configurable if it mis-flags.
const natAggregateFpThreshold = 8

// conf is a signature's confidence, encoded so a bigger number is more certain.
// The digit is also the lexical rank prefix for the OS MAX (high '9' > med '5').
type conf uint8

const (
	confLow  conf = 1 // corroborating only — NEVER auto-surfaced (dropped from concat)
	confMed  conf = 5 // safe to surface as a chip
	confHigh conf = 9 // sole-basis OK
)

// sigRule maps a characteristic domain substring to one facet label at a
// confidence. First match wins per row inside a CASE; across a client the OS
// resolves by conf-ranked MAX and the set facets by GROUP_CONCAT(DISTINCT).
// Matching is question_name LIKE '%match%' (substring, no port visibility).
//
// ponytail: naive substring match; refine only if it visibly mislabels.
//
//nolint:gochecknoglobals // static lookup table
var sigRules = []sigRule{
	// ── S-OS (single-valued, conf-ranked) ──
	{"push.apple.com", "os", "iOS/macOS", confHigh},
	{"gs-loc.apple.com", "os", "iOS", confMed},
	{"mesu.apple.com", "os", "iOS/macOS", confMed},
	{"swscan.apple.com", "os", "macOS", confHigh},
	{"mtalk.google.com", "os", "Android", confHigh},
	{"android.googleapis.com", "os", "Android", confHigh},
	{"connectivitycheck.gstatic.com", "os", "Android", confMed},
	{"dns.msftncsi.com", "os", "Windows", confHigh},
	{"settings-win.data.microsoft.com", "os", "Windows", confHigh},
	{"windowsupdate.com", "os", "Windows", confHigh},
	{"ntp.ubuntu.com", "os", "Linux", confMed},
	{"archlinux.org", "os", "Linux", confMed},

	// ── S-VENDOR / S-MODEL (multi-valued, conf>=med surfaces) ──
	{"apple.com", "vendor", "Apple", confLow},   // dropped from chips — too broad
	{"googleapis.com", "vendor", "Google", confLow},
	{"microsoft.com", "vendor", "Microsoft", confLow},
	{"samsungcloud.com", "vendor", "Samsung", confMed},
	{"xboxlive.com", "vendor", "Microsoft", confHigh},
	{"xboxlive.com", "model", "Xbox", confHigh},
	{"playstation.net", "vendor", "Sony", confHigh},
	{"playstation.net", "model", "PlayStation", confHigh},
	{"nintendo.net", "vendor", "Nintendo", confHigh},
	{"roku.com", "vendor", "Roku", confHigh},
	{"tuyaus.com", "vendor", "Tuya", confHigh},
	{"tuyaeu.com", "vendor", "Tuya", confHigh},
	{"aqara.com", "vendor", "Aqara", confHigh},
	{"hik-connect.com", "vendor", "Hikvision", confHigh},
	{"axis.com", "vendor", "Axis", confHigh},
	{"axis.com", "model", "Camera", confHigh},
	{"sonos.com", "vendor", "Sonos", confHigh},
	{"meethue.com", "vendor", "PhilipsHue", confHigh},
	{"shelly.cloud", "vendor", "Shelly", confHigh},
	{"ldm-devapi.sunnyportal.com", "vendor", "SMA", confHigh},
	{"ldm-devapi.sunnyportal.com", "model", "Inverter", confHigh},
	{"fusionsolar.huawei.com", "vendor", "Huawei", confHigh},
	{"fusionsolar.huawei.com", "model", "Inverter", confHigh},
	{"googlecast", "vendor", "Google", confHigh},
	{"googlecast", "model", "Chromecast", confHigh},

	// ── S-APP (multi-valued, conf>=med surfaces) ──
	{"netflix.com", "app", "Netflix", confHigh},
	{"nflxvideo.net", "app", "Netflix", confHigh},
	{"youtube.com", "app", "YouTube", confHigh},
	{"googlevideo.com", "app", "YouTube", confHigh},
	{"spotify.com", "app", "Spotify", confMed},
	{"discord.com", "app", "Discord", confHigh},
	{"whatsapp.net", "app", "WhatsApp", confHigh},
	{"telegram.org", "app", "Telegram", confHigh},
	{"steamcontent.com", "app", "Steam", confHigh},
	{"epicgames.com", "app", "EpicGames", confHigh},
	{"zoom.us", "app", "Zoom", confHigh},
	{"teams.microsoft.com", "app", "Teams", confHigh},
}

type sigRule struct {
	match string
	facet string // "os", "vendor", "model", "app"
	label string
	conf  conf
}

// whenClause is one "WHEN question_name LIKE '%match%' THEN '<then>'" fragment.
// match/then are compile-time constants (no user input), inlined safely.
func whenClause(match, then string) string {
	return " WHEN question_name LIKE '%" + strings.ReplaceAll(match, "'", "''") +
		"%' THEN '" + strings.ReplaceAll(then, "'", "''") + "'"
}

// osCaseSQL yields MAX(CASE ... END) over the os rules. Each match resolves to
// a lexically-sortable 'N|label' ('9'=high, '5'=med), so the plain MAX picks
// the highest-confidence guess; the caller strips the 'N|' prefix in Go.
// Rules are emitted high-conf first so the first CASE hit within a row is also
// the strongest for that row.
func osCaseSQL() string {
	var b strings.Builder

	b.WriteString("MAX(CASE")

	for _, r := range sortedByConf(facetRules("os", confLow)) {
		b.WriteString(whenClause(r.match, fmt.Sprintf("%d|%s", r.conf, r.label)))
	}

	b.WriteString(" END)")

	return b.String()
}

// facetConcatSQL yields GROUP_CONCAT(DISTINCT CASE ... END) over the rules for
// one multi-valued facet, built ONLY from conf>=med rules — low-confidence CDN
// signatures (apple.com, googleapis.com, microsoft.com) tag nearly every device
// and must never auto-surface as a chip (blueprint R4). Default ',' separator
// (sqlite forbids a custom separator with DISTINCT); labels carry no commas.
func facetConcatSQL(facet string) string {
	var b strings.Builder

	b.WriteString("GROUP_CONCAT(DISTINCT CASE")

	for _, r := range facetRules(facet, confMed) {
		b.WriteString(whenClause(r.match, r.label))
	}

	b.WriteString(" END)")

	return b.String()
}

// facetRules returns the rules for one facet at or above minConf.
func facetRules(facet string, minConf conf) []sigRule {
	var out []sigRule

	for _, r := range sigRules {
		if r.facet == facet && r.conf >= minConf {
			out = append(out, r)
		}
	}

	return out
}

func sortedByConf(rules []sigRule) []sigRule {
	sort.SliceStable(rules, func(i, j int) bool { return rules[i].conf > rules[j].conf })

	return rules
}

// clientEnrich holds the derived identity fields for one client_name.
type clientEnrich struct {
	IPs          []string
	NatAggregate bool
	FpCount      int
	OS           string
	Vendor       []string
	Model        []string
	Apps         []string
	DeviceGuess  string // legacy single-label field: OS, else first vendor, else first app
	Shared       bool
	SharedLabel  string // "shared / N devices" when NatAggregate suppresses facets
}

type clientEnrichRow struct {
	Name        string `gorm:"column:name"`
	IPs         string `gorm:"column:ips"` // GROUP_CONCAT(DISTINCT client_ip), comma-joined
	FpCount     int    `gorm:"column:fp_count"`
	OSGuess     string `gorm:"column:os_guess"`     // 'N|label' from the conf-ranked MAX
	VendorGuess string `gorm:"column:vendor_guess"` // comma-joined labels
	ModelGuess  string `gorm:"column:model_guess"`
	AppGuess    string `gorm:"column:app_guess"`
}

func (row clientEnrichRow) toEnrich() clientEnrich {
	e := clientEnrich{
		IPs:          splitCSV(row.IPs),
		FpCount:      row.FpCount,
		NatAggregate: row.FpCount >= natAggregateFpThreshold,
	}

	// NAT gate (R3): a shared identity's union-of-all facets is noise — suppress
	// everything and relabel as a device count.
	if e.NatAggregate {
		e.Shared = true
		e.SharedLabel = fmt.Sprintf("shared / %d devices", row.FpCount)

		return e
	}

	e.OS = stripConfPrefix(row.OSGuess)
	e.Vendor = splitCSV(row.VendorGuess)
	e.Model = splitCSV(row.ModelGuess)
	e.Apps = splitCSV(row.AppGuess)
	e.DeviceGuess = firstNonEmpty(e.OS, first(e.Vendor), first(e.Apps))

	return e
}

func stripConfPrefix(s string) string {
	if i := strings.IndexByte(s, '|'); i >= 0 {
		return s[i+1:]
	}

	return s
}

func splitCSV(s string) []string {
	var out []string

	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}

	return out
}

func first(ss []string) string {
	if len(ss) > 0 {
		return ss[0]
	}

	return ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}

	return ""
}

// enrichSelectCols are the derived facet columns shared by the list and single
// enrich SELECTs (same GROUP BY client_name scan, just more projected columns).
func enrichSelectCols() string {
	return `GROUP_CONCAT(DISTINCT client_ip) AS ips,
		COUNT(DISTINCT NULLIF(fp_hash,'')) AS fp_count,
		` + osCaseSQL() + ` AS os_guess,
		` + facetConcatSQL("vendor") + ` AS vendor_guess,
		` + facetConcatSQL("model") + ` AS model_guess,
		` + facetConcatSQL("app") + ` AS app_guess`
}

// enrichClients returns the derived identity fields per client_name over the
// window, from a single grouped scan of the raw log_entries table.
func (r *Reader) enrichClients(from, to time.Time) (map[string]clientEnrich, error) {
	var rows []clientEnrichRow

	err := r.db.Raw(`SELECT client_name AS name, `+enrichSelectCols()+`
		FROM log_entries WHERE request_ts >= ? AND request_ts <= ? AND decoy = 0
		GROUP BY client_name`, from, to).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	out := make(map[string]clientEnrich, len(rows))
	for _, row := range rows {
		out[row.Name] = row.toEnrich()
	}

	return out, nil
}

// enrichClient returns the derived identity fields for a single client_name.
func (r *Reader) enrichClient(name string, from, to time.Time) (clientEnrich, error) {
	var row clientEnrichRow

	err := r.db.Raw(`SELECT client_name AS name, `+enrichSelectCols()+`
		FROM log_entries WHERE client_name = ? AND request_ts >= ? AND request_ts <= ? AND decoy = 0`,
		name, from, to).Scan(&row).Error
	if err != nil {
		return clientEnrich{}, err
	}

	return row.toEnrich(), nil
}
