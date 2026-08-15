package querylog

import (
	"strings"
	"time"
)

// Client identity enrichment derived entirely from the already-persisted raw
// log_entries rows (client_ip + fp_hash + question_name). No schema change, no
// migration: the aggregate tables that back ClientList/ClientDetail dropped
// client_ip and can't count distinct fingerprints, so this reads log_entries
// directly. Client lists are small, so one extra windowed SELECT is cheap.

// natAggregateFpThreshold: a client (one client_name/IP) showing at least this
// many DISTINCT fingerprint hashes is almost certainly many devices behind one
// identity (a router/hairpin, e.g. 10.128.48.1), so we flag it.
//
// ponytail: fixed distinct-fp threshold; make configurable if it mis-flags.
const natAggregateFpThreshold = 8

// deviceGuessRules maps a characteristic domain suffix to a device/vendor label.
// First match wins per row; ties across a client resolve by MAX(label) in SQL.
// Keep it small and easy to extend — add a {suffix,label} line.
//
// ponytail: naive substring match + lexicographic tie-break on multi-match.
// Good enough for a heuristic badge; refine only if it visibly mislabels.
//
//nolint:gochecknoglobals // static lookup table
var deviceGuessRules = []struct{ suffix, label string }{
	{"courier.push.apple.com", "Apple device"},
	{"push.apple.com", "Apple device"},
	{"time.apple.com", "Apple device"},
	{"mtalk.google.com", "Android"},
	{"android.googleapis.com", "Android"},
	{"xboxlive.com", "Xbox"},
	{"playstation.net", "PlayStation"},
	{"nintendo.net", "Nintendo Switch"},
	{"samsungcloud", "Samsung"},
	{"samsungqbe", "Samsung"},
	{"amazonalexa", "Amazon Echo"},
	{"device-metrics-us.amazon", "Amazon Echo"},
	{"roku.com", "Roku"},
	{"spotify.com", "Spotify client"},
	{"windowsupdate.com", "Windows"},
	{"dns.msftncsi.com", "Windows"},
}

// deviceGuessCaseSQL builds a single CASE expression that yields a device label
// for a log_entries row from its question_name. Suffixes are compile-time
// constants (no user input), so they're inlined safely.
func deviceGuessCaseSQL() string {
	var b strings.Builder

	b.WriteString("CASE")

	for _, r := range deviceGuessRules {
		b.WriteString(" WHEN question_name LIKE '%")
		b.WriteString(r.suffix)
		b.WriteString("%' THEN '")
		b.WriteString(strings.ReplaceAll(r.label, "'", "''"))
		b.WriteString("'")
	}

	b.WriteString(" END")

	return b.String()
}

// clientEnrich holds the derived identity fields for one client_name.
type clientEnrich struct {
	IPs          []string
	NatAggregate bool
	FpCount      int
	DeviceGuess  string
}

type clientEnrichRow struct {
	Name        string `gorm:"column:name"`
	IPs         string `gorm:"column:ips"` // GROUP_CONCAT(DISTINCT client_ip), comma-joined
	FpCount     int    `gorm:"column:fp_count"`
	DeviceGuess string `gorm:"column:device_guess"`
}

func (row clientEnrichRow) toEnrich() clientEnrich {
	var ips []string

	for _, ip := range strings.Split(row.IPs, ",") {
		if ip = strings.TrimSpace(ip); ip != "" {
			ips = append(ips, ip)
		}
	}

	return clientEnrich{
		IPs:          ips,
		NatAggregate: row.FpCount >= natAggregateFpThreshold,
		FpCount:      row.FpCount,
		DeviceGuess:  row.DeviceGuess,
	}
}

// enrichClients returns the derived identity fields per client_name over the
// window, from a single grouped scan of the raw log_entries table.
func (r *Reader) enrichClients(from, to time.Time) (map[string]clientEnrich, error) {
	var rows []clientEnrichRow

	// one windowed SELECT: distinct IPs, distinct-fp count, best device guess
	err := r.db.Raw(`SELECT client_name AS name,
		GROUP_CONCAT(DISTINCT client_ip) AS ips,
		COUNT(DISTINCT NULLIF(fp_hash,'')) AS fp_count,
		MAX(`+deviceGuessCaseSQL()+`) AS device_guess
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

	err := r.db.Raw(`SELECT client_name AS name,
		GROUP_CONCAT(DISTINCT client_ip) AS ips,
		COUNT(DISTINCT NULLIF(fp_hash,'')) AS fp_count,
		MAX(`+deviceGuessCaseSQL()+`) AS device_guess
		FROM log_entries WHERE client_name = ? AND request_ts >= ? AND request_ts <= ? AND decoy = 0`,
		name, from, to).Scan(&row).Error
	if err != nil {
		return clientEnrich{}, err
	}

	return row.toEnrich(), nil
}
