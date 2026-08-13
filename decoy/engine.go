package decoy

import (
	"context"
	"math/rand"
	"net"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/metrics"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/querylog"
	"github.com/0xERR0R/blocky/util"
)

//nolint:gochecknoglobals
var decoyQueriesTotal = promauto.With(metrics.Reg).NewCounter(prometheus.CounterOpts{
	Name: "blocky_decoy_queries_total",
	Help: "Total number of synthetic decoy/noise queries emitted",
})

// decoyClientIP is the pseudo-client all decoy queries appear to come from
// (loopback), so they never look like a real LAN client in the log.
const decoyClientIP = "127.0.0.1"

// splitClientIPs (technique 4) is the pool of pseudo-client identities decoys
// rotate through so per-client group routing can diverge. See clientIP.
//
//nolint:gochecknoglobals
var splitClientIPs = []string{"127.0.0.1", "127.0.0.2", "10.0.0.254", "192.168.255.254"}

// clusterCompanions (technique 6) are common third-party hosts a real page load
// pulls alongside a first-party domain, making sibling-cluster decoys realistic.
//
//nolint:gochecknoglobals
var clusterCompanions = []string{
	"fonts.googleapis.com", "fonts.gstatic.com", "cdn.jsdelivr.net",
	"www.google-analytics.com", "ajax.googleapis.com", "cdnjs.cloudflare.com",
}

// ResolveFunc runs a request through the server's resolver chain (same path
// real queries take, so decoys follow the active group strategy/recursion).
type ResolveFunc func(ctx context.Context, req *model.Request) (*model.Response, error)

// Engine emits background noise queries at a randomized rate, mixing replayed
// real queries with entries from the static list. Every emitted request is
// marked Bypass (skip cache) and Decoy (excluded from dashboards).
type Engine struct {
	cfg     config.DecoyConfig
	source  *querylog.DecoySource
	resolve ResolveFunc
	logger  *logrus.Entry
	rnd     *rand.Rand
	now     func() time.Time // injectable for tests
}

func NewEngine(cfg config.DecoyConfig, source *querylog.DecoySource, resolve ResolveFunc) *Engine {
	return &Engine{
		cfg:     cfg,
		source:  source,
		resolve: resolve,
		logger:  log.PrefixedLog("decoy"),
		rnd:     rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec // noise timing, not crypto
		now:     time.Now,
	}
}

// Seed loads the embedded list into the source if the table is empty.
func (e *Engine) Seed() error {
	r, err := openList()
	if err != nil {
		return err
	}
	defer r.Close()

	n, err := e.source.SeedIfEmpty(r)
	if err != nil {
		return err
	}

	if n > 0 {
		e.logger.Infof("seeded %d decoy domains", n)
	}

	return nil
}

// Run seeds the list, then emits decoy queries until ctx is cancelled. Inter-
// arrival times are exponentially distributed (Poisson process) around the
// configured mean rate — a clean fixed tick would itself be a fingerprint.
func (e *Engine) Run(ctx context.Context) {
	if err := e.Seed(); err != nil {
		e.logger.WithError(err).Error("can't seed decoy list; decoy engine not started")

		return
	}

	e.logger.Infof("decoy engine started (%.1f queries/min, active %02d:00-%02d:00)",
		e.cfg.QueriesPerMinute, e.cfg.ActiveHoursStart, e.cfg.ActiveHoursEnd)

	timer := time.NewTimer(e.nextInterval())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if e.withinActiveHours(e.now()) {
				e.emit(ctx)
			}

			timer.Reset(e.nextInterval())
		}
	}
}

// nextInterval draws an exponential inter-arrival time for the configured rate,
// scaled by the diurnal shape factor so the noise floor tracks real activity.
func (e *Engine) nextInterval() time.Duration {
	qpm := e.cfg.QueriesPerMinute
	if qpm <= 0 {
		qpm = 1
	}

	qpm *= e.diurnalFactor()
	if qpm < 0.01 { //nolint:mnd // floor so meanSeconds never explodes
		qpm = 0.01
	}

	meanSeconds := 60.0 / qpm

	return time.Duration(e.rnd.ExpFloat64() * meanSeconds * float64(time.Second))
}

// diurnalFactor (technique 1) scales the base rate by how busy the current hour
// is relative to the day's average. Cold start / disabled → 1.
func (e *Engine) diurnalFactor() float64 {
	if !e.cfg.DiurnalShaping || e.source == nil {
		return 1
	}

	counts, err := e.source.HourlyRealCounts()
	if err != nil {
		return 1 // no aggregate table yet — treat as cold start
	}

	return diurnalShape(counts, e.now().UTC().Hour())
}

// diurnalShape = thisHourShare / averageHourShare, clamped to [0.2, 3]. Pure so
// it is unit-testable without a database.
func diurnalShape(counts [24]int64, hour int) float64 {
	var total int64
	for _, c := range counts {
		total += c
	}

	if total == 0 {
		return 1
	}

	avg := float64(total) / 24.0

	f := float64(counts[hour]) / avg

	switch {
	case f < 0.2: //nolint:mnd // documented clamp bounds
		return 0.2
	case f > 3: //nolint:mnd // documented clamp bounds
		return 3
	default:
		return f
	}
}

// withinActiveHours reports whether t's hour is in [start, end). end==24 means
// midnight (whole day when start==0).
func (e *Engine) withinActiveHours(t time.Time) bool {
	h := t.Hour()

	return h >= e.cfg.ActiveHoursStart && h < e.cfg.ActiveHoursEnd
}

// decoyQuery is one synthetic lookup to emit. qclass 0 means default IN.
type decoyQuery struct {
	name   string
	qtype  uint16
	qclass uint16
	replay bool // sourced from the real-query replay pool
}

// emit builds the decoy queries for one emission and resolves each through the
// chain. Normally one query; techniques 5/6 may replace or fan it out.
func (e *Engine) emit(ctx context.Context) {
	q := e.nextQuery()
	if q.name == "" {
		return
	}

	// technique 5: NXDOMAIN/miss chaff — a random label under a real domain.
	if e.rndPct(e.cfg.MissChaffPct) {
		q = decoyQuery{name: e.randLabel() + "." + q.name, qtype: dns.TypeA}
	} else if q.replay && e.cfg.ReplayMutate && e.rnd.Intn(2) == 0 { //nolint:mnd // 0.5 mutation probability
		q = e.mutate(q) // technique 2: never a byte-identical echo
	}

	queries := []decoyQuery{q}
	if e.rndPct(e.cfg.ClusterPct) {
		queries = e.clusterOf(q) // technique 6: small related burst
	}

	for _, cq := range queries {
		e.resolveOne(ctx, cq)
	}
}

// resolveOne builds and resolves a single decoy request, stamping the sampled
// real fingerprint (technique 3) and split-upstream client identity (technique
// 4). Every request is Bypass + Decoy.
func (e *Engine) resolveOne(ctx context.Context, q decoyQuery) {
	msg := util.NewMsgWithQuestion(dns.Fqdn(q.name), dns.Type(q.qtype))
	if q.qclass != 0 {
		msg.Question[0].Qclass = q.qclass
	}

	req := &model.Request{
		ClientIP:  e.clientIP(),
		Req:       msg,
		Protocol:  model.RequestProtocolUDP,
		RequestTS: e.now(),
		Bypass:    true,
		Decoy:     true,
	}

	e.applyFingerprint(req)

	if _, err := e.resolve(ctx, req); err != nil {
		e.logger.WithError(err).Debug("decoy query failed")
	}

	decoyQueriesTotal.Inc()
}

// nextQuery weighted-chooses a source and returns a domain + qtype. Replay is
// preferred by ReplayWeight:ListWeight, but an empty replay pool (cold start)
// falls through to the list, so early on it is effectively 100% list.
func (e *Engine) nextQuery() decoyQuery {
	if e.chooseReplay() {
		if q, err := e.source.SampleRecentReal(1); err != nil {
			e.logger.WithError(err).Debug("replay sample failed")
		} else if len(q) > 0 {
			return decoyQuery{name: q[0].Name, qtype: qtypeFromString(q[0].Qtype), replay: true}
		}
	}

	domain, err := e.source.SampleList()
	if err != nil {
		e.logger.WithError(err).Debug("list sample failed")

		return decoyQuery{}
	}

	return decoyQuery{name: domain, qtype: e.randListQtype()}
}

// clientIP returns the pseudo-client for a decoy. Technique 4 (split-upstream)
// varies it across a small pool so per-client group routing can send decoys
// down a different group than the real client would use.
//
// ponytail: honest limitation — decoys traverse the whole resolver chain via
// s.resolve, so we cannot pick a specific upstream/group per query. Varying the
// client IP only diverges routing when the operator has client-group rules
// covering these addresses; otherwise decoys stay on the default group (still a
// different group than real LAN clients, which is the baseline win). True
// per-query upstream selection would need a decoy-aware tag in the resolver
// tree — deferred as too invasive.
func (e *Engine) clientIP() net.IP {
	if !e.cfg.SplitUpstream {
		return net.ParseIP(decoyClientIP)
	}

	return net.ParseIP(splitClientIPs[e.rnd.Intn(len(splitClientIPs))])
}

// applyFingerprint (technique 3) stamps the EDNS shape of a sampled real query
// onto the decoy so synthetic queries aren't filterable by "all look default".
func (e *Engine) applyFingerprint(req *model.Request) {
	if !e.cfg.FingerprintMatch {
		return
	}

	fp, err := e.source.SampleRealFingerprint()
	if err != nil || !fp.HadEDNS0 {
		return // cold start / no EDNS on the sampled client — leave the plain query
	}

	req.Req.SetEdns0(fp.EDNSUDPSize, fp.DO)
	req.Fingerprint.HadEDNS0 = true
	req.Fingerprint.EDNSUDPSize = fp.EDNSUDPSize
	req.Fingerprint.DO = fp.DO
}

// mutate returns a variant of a replayed query that is never byte-identical to
// the real echo: qtype flip, subdomain prepend, 0x20 case, or (rarely) class.
func (e *Engine) mutate(q decoyQuery) decoyQuery {
	switch r := e.rnd.Intn(10); { //nolint:mnd // branch weights, see cases
	case r < 4: // flip A<->AAAA
		if q.qtype == dns.TypeAAAA {
			q.qtype = dns.TypeA
		} else {
			q.qtype = dns.TypeAAAA
		}
	case r < 7: // prepend a plausible subdomain label
		q.name = e.randSubLabel() + "." + q.name
	case r < 9: // 0x20 mixed-case encoding
		q.name = e.randomize0x20(q.name)
	default: // rare class change
		q.qclass = dns.ClassCHAOS
	}

	return q
}

// clusterOf (technique 6) expands one query into a small capped burst of related
// lookups: the domain, its www + AAAA, and maybe a common third-party companion.
func (e *Engine) clusterOf(q decoyQuery) []decoyQuery {
	candidates := []decoyQuery{
		q,
		{name: "www." + q.name, qtype: dns.TypeA},
		{name: q.name, qtype: dns.TypeAAAA},
		{name: clusterCompanions[e.rnd.Intn(len(clusterCompanions))], qtype: dns.TypeA},
	}

	n := 2 + e.rnd.Intn(3) //nolint:mnd // burst size 2..4, capped
	if n > len(candidates) {
		n = len(candidates)
	}

	return candidates[:n]
}

// rndPct reports true with probability pct/100 (pct is validated to [0,100]).
func (e *Engine) rndPct(pct uint) bool {
	if pct == 0 {
		return false
	}

	return uint(e.rnd.Intn(100)) < pct //nolint:mnd,gosec // percent roll, not crypto
}

// randLabel returns a random short DNS label (likely-nonexistent, for miss chaff).
func (e *Engine) randLabel() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

	n := 6 + e.rnd.Intn(5) //nolint:mnd // 6..10 chars

	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[e.rnd.Intn(len(alphabet))]
	}

	return string(b)
}

// randSubLabel returns a plausible CDN/service subdomain label + short token.
func (e *Engine) randSubLabel() string {
	prefixes := []string{"www", "cdn", "api", "m", "static", "img"}

	return prefixes[e.rnd.Intn(len(prefixes))] + e.randToken()
}

func (e *Engine) randToken() string {
	if e.rnd.Intn(2) == 0 {
		return "" // sometimes a bare prefix (www.<d>)
	}

	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

	n := 1 + e.rnd.Intn(3) //nolint:mnd // 1..3 chars

	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[e.rnd.Intn(len(alphabet))]
	}

	return string(b)
}

// randomize0x20 randomly upper/lower-cases letters, forcing at least one flip so
// the result differs from the input whenever it contains a letter.
func (e *Engine) randomize0x20(name string) string {
	b := []byte(name)

	firstLetter := -1

	for i, c := range b {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			if firstLetter < 0 {
				firstLetter = i
			}

			if e.rnd.Intn(2) == 0 {
				b[i] ^= 0x20 // toggle case bit
			}
		}
	}

	if firstLetter >= 0 {
		b[firstLetter] ^= 0x20 // guarantee non-identical
	}

	return string(b)
}

// chooseReplay flips a weighted coin: replay with probability
// ReplayWeight/(ReplayWeight+ListWeight).
func (e *Engine) chooseReplay() bool {
	total := e.cfg.ReplayWeight + e.cfg.ListWeight
	if total == 0 {
		return false
	}

	return uint(e.rnd.Intn(int(total))) < e.cfg.ReplayWeight
}

// randListQtype picks A or AAAA for list-sourced queries.
func (e *Engine) randListQtype() uint16 {
	if e.rnd.Intn(2) == 0 {
		return dns.TypeAAAA
	}

	return dns.TypeA
}

func qtypeFromString(s string) uint16 {
	if t, ok := dns.StringToType[s]; ok {
		return t
	}

	return dns.TypeA
}
