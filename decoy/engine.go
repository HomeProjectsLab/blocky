package decoy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/publicsuffix"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/metrics"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/querylog"
	"github.com/0xERR0R/blocky/util"
)

// Reactive-obfuscation tuning (technique 1 live-volume + technique 6 companions).
const (
	// realWindow is the rolling window over which live real QPS is measured.
	realWindow = 60 * time.Second
	// reactiveJitterQPM is the ± masking term (queries/min) added to the live
	// rate each interval, so the decoy rate never exactly equals the real rate.
	reactiveJitterQPM = 3
	// minLiveEvents is how many real queries must sit in the window before the
	// live rate is trusted; below it we fall back to the historical diurnal path.
	minLiveEvents = 2
	// minReactiveQPM floors the reactive rate so a lull with a little live signal
	// still emits some cover and the interval never explodes.
	minReactiveQPM = 0.5
	// companionDelayMinMs/SpreadMs bound the randomized inter-companion delay,
	// mimicking sub-resource timing of a page load (tens to hundreds of ms).
	companionDelayMinMs    = 40
	companionDelaySpreadMs = 300
	// ttlSuppressSweep is the ttlSuppress map size past which noteTTL sweeps expired
	// entries, so the map can't grow unbounded on a long-lived process.
	ttlSuppressSweep = 4096
	// ttlFallbackSecs is the suppression window applied when a decoy's response
	// carries no answer TTL (NXDOMAIN / empty) — a short negative-cache stand-in.
	ttlFallbackSecs = 30
)

// decoy source selectors (T3 three-way weighted mix in nextQuery).
const (
	srcReplay = iota // recent real-query replay pool (7-day window)
	srcCorpus        // persistent all-time visited-domains corpus
	srcList          // public Tranco static list
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

// clusterCompanions (technique 6/T8) are common third-party hosts a real page
// load pulls alongside a first-party domain. A large, real-world pool (CDN,
// analytics, fonts, tag managers, ad/RTB) — companionsFor/clusterOf draw a
// random subset in random order so a decoy burst carries no fixed template
// signature of this fork.
//
//nolint:gochecknoglobals
var clusterCompanions = []string{
	// fonts
	"fonts.googleapis.com", "fonts.gstatic.com", "use.typekit.net", "p.typekit.net",
	// script/asset CDNs
	"cdn.jsdelivr.net", "cdnjs.cloudflare.com", "ajax.googleapis.com", "unpkg.com",
	"code.jquery.com", "stackpath.bootstrapcdn.com", "maxcdn.bootstrapcdn.com",
	// analytics / tag managers
	"www.google-analytics.com", "www.googletagmanager.com", "ssl.google-analytics.com",
	"analytics.tiktok.com", "connect.facebook.net", "static.hotjar.com",
	"cdn.segment.com", "cdn.amplitude.com", "js.sentry-cdn.com", "browser.sentry-cdn.com",
	"px.ads.linkedin.com", "snap.licdn.com",
	// ads / RTB
	"pagead2.googlesyndication.com", "securepubads.g.doubleclick.net",
	"static.doubleclick.net", "adservice.google.com", "c.amazon-adsystem.com",
	// media / object CDNs
	"i.ytimg.com", "s.ytimg.com", "res.cloudinary.com", "imgix.net",
	"cdn.shopify.com", "assets.squarespace.com", "d3js.org",
	// consent / misc widgets
	"cdn.cookielaw.org", "widget.intercom.io", "js.stripe.com", "www.gstatic.com",
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
	hub     *querylog.Hub // live real-query tap; nil (non-sqlite) → historical fallback
	logger  *logrus.Entry
	rnd     *rand.Rand
	now     func() time.Time // injectable for tests

	realMu    sync.Mutex  // guards realTimes (tap goroutine writes, emit loop reads)
	realTimes []time.Time // timestamps of recent real queries within realWindow

	ttlMu       sync.Mutex           // guards ttlSuppress (T5 shadow-TTL)
	ttlSuppress map[string]time.Time // (name/qtype) -> earliest re-egress time

	cookieMu sync.Mutex        // guards cookies (T13 per-client stable EDNS cookie)
	cookies  map[string]string // pseudo-client IP -> stable hex cookie

	edgeMu   sync.Mutex // guards the per-day active-hours edge jitter (T10)
	edgeDay  string     // yyyy-mm-dd the current edge offsets were drawn for
	startOff int        // today's start-edge jitter, minutes
	endOff   int        // today's end-edge jitter, minutes
}

func NewEngine(cfg config.DecoyConfig, source *querylog.DecoySource, resolve ResolveFunc) *Engine {
	return &Engine{
		cfg:         cfg,
		source:      source,
		resolve:     resolve,
		logger:      log.PrefixedLog("decoy"),
		rnd:         rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec // noise timing, not crypto
		now:         time.Now,
		ttlSuppress: map[string]time.Time{},
		cookies:     map[string]string{},
	}
}

// SetHub wires the live query-log tap so the engine can react to real traffic
// (live-volume tracking + browse-triggered companions). A nil hub (non-sqlite
// query log) leaves the engine on the historical diurnal + timer-cluster path.
func (e *Engine) SetHub(h *querylog.Hub) { e.hub = h }

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

	if e.hub != nil {
		ch, unsub := e.hub.Subscribe()
		defer unsub()

		go e.tapLoop(ctx, ch)
	}

	timer := time.NewTimer(e.nextInterval())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			// T10: never gate fully to zero — emit every tick. Outside active
			// hours effectiveQPM (via nextInterval) collapses to the low
			// always-on floor rather than stopping, so an observer can't read the
			// window edges as a clean on/off step.
			e.emit(ctx)

			timer.Reset(e.nextInterval())
		}
	}
}

// nextInterval draws an exponential inter-arrival time (Poisson process) for the
// current effective rate.
func (e *Engine) nextInterval() time.Duration {
	qpm := e.effectiveQPM()
	if qpm < 0.01 { //nolint:mnd // floor so meanSeconds never explodes
		qpm = 0.01
	}

	meanSeconds := 60.0 / qpm

	return time.Duration(e.rnd.ExpFloat64() * meanSeconds * float64(time.Second))
}

// effectiveQPM is the decoy rate for the next interval. With ReactiveVolume on
// and enough live signal it tracks the recent real QPS ± a random masking term
// (see reactiveQPM); otherwise it uses the configured base scaled by the 7-day
// historical diurnal shape (the cold-start / quiet fallback, today's behaviour).
//
// ponytail: reactive volume inherently leaks the real activity LEVEL to an
// on-path observer — the decoy rate rises and falls with real QPS. That is the
// deliberate trade: we hide WHICH queries are real, not THAT the box is busy.
// Known design bound; hiding the level too would need constant-rate cover
// traffic (a fixed bandwidth cost we chose not to pay on a home box).
func (e *Engine) effectiveQPM() float64 {
	// T10: outside active hours we don't stop — we drop to a low always-on floor.
	if !e.withinActiveHours(e.now()) {
		return e.cfg.OffHoursFloorQPM
	}

	base := e.cfg.QueriesPerMinute
	if base <= 0 {
		base = 1
	}

	if e.cfg.ReactiveVolume {
		if n, qpm := e.reactiveQPM(); n >= minLiveEvents {
			return qpm
		}
	}

	return base * e.diurnalFactor()
}

// reactiveQPM returns (live real-query count in the window, target decoy QPM).
// The target is the live real rate plus a per-interval ± random(reactiveJitterQPM)
// queries/min term — an additive mask so the decoy rate is never exactly the real
// rate — floored at minReactiveQPM.
func (e *Engine) reactiveQPM() (int, float64) {
	n := e.recentRealCount()

	realQPM := float64(n) / realWindow.Seconds() * 60.0

	jitter := float64(e.rnd.Intn(2*reactiveJitterQPM+1) - reactiveJitterQPM) //nolint:gosec // noise, not crypto

	qpm := realQPM + jitter
	if qpm < minReactiveQPM {
		qpm = minReactiveQPM
	}

	return n, qpm
}

// tapLoop consumes the marshalled QueryItem stream and reacts to each real query.
func (e *Engine) tapLoop(ctx context.Context, ch <-chan []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-ch:
			e.tap(ctx, data)
		}
	}
}

// tap reacts to one query-log event: it counts real queries toward the live-QPS
// window and may fire a companion cluster. Decoy-flagged events (our own decoys
// and companions carry Decoy=true) are dropped here — the feedback guard that
// stops companions-of-companions and keeps the real-QPS counter honest.
func (e *Engine) tap(ctx context.Context, data []byte) {
	var item struct {
		Decoy    bool   `json:"decoy"`
		Question string `json:"question"`
		Qtype    string `json:"qtype"`
	}

	if json.Unmarshal(data, &item) != nil {
		return
	}

	if item.Decoy || item.Question == "" {
		return // feedback guard + ignore empty-question rows
	}

	e.recordReal()
	e.maybeCompanion(ctx, item.Question)
}

// recordReal stamps a real query into the rolling window.
func (e *Engine) recordReal() {
	e.realMu.Lock()
	e.realTimes = append(e.realTimes, e.now())
	e.pruneRealLocked()
	e.realMu.Unlock()
}

// recentRealCount prunes aged entries and returns how many real queries fall in
// the last realWindow.
func (e *Engine) recentRealCount() int {
	e.realMu.Lock()
	e.pruneRealLocked()
	n := len(e.realTimes)
	e.realMu.Unlock()

	return n
}

// pruneRealLocked drops timestamps older than the window. realTimes is appended
// in now() order so the aged entries are a contiguous prefix.
func (e *Engine) pruneRealLocked() {
	cutoff := e.now().Add(-realWindow)

	i := 0
	for i < len(e.realTimes) && e.realTimes[i].Before(cutoff) {
		i++
	}

	e.realTimes = e.realTimes[i:]
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

// withinActiveHours reports whether t falls in the active window [start, end),
// with the window edges jittered by ± ActiveHoursEdgeJitterMin minutes, redrawn
// once per calendar day (T10), so the boundary isn't a clean step an observer can
// calibrate. A 24h config (start=0, end=24) is always active — no edge to jitter.
func (e *Engine) withinActiveHours(t time.Time) bool {
	if e.cfg.ActiveHoursStart == 0 && e.cfg.ActiveHoursEnd == 24 {
		return true
	}

	startMin, endMin := e.activeEdges(t)
	m := t.Hour()*60 + t.Minute()

	return m >= startMin && m < endMin
}

// activeEdges returns today's jittered [start, end) window edges in minutes.
// Offsets are drawn once per calendar day and cached, so all checks within a day
// agree on the same (jittered) boundary.
func (e *Engine) activeEdges(t time.Time) (int, int) {
	startMin := e.cfg.ActiveHoursStart * 60
	endMin := e.cfg.ActiveHoursEnd * 60

	j := e.cfg.ActiveHoursEdgeJitterMin
	if j <= 0 {
		return startMin, endMin
	}

	day := t.Format("2006-01-02")

	e.edgeMu.Lock()
	if day != e.edgeDay {
		e.edgeDay = day
		e.startOff = e.rnd.Intn(2*j+1) - j //nolint:gosec // noise timing, not crypto
		e.endOff = e.rnd.Intn(2*j+1) - j   //nolint:gosec // noise timing, not crypto
	}
	so, eo := e.startOff, e.endOff
	e.edgeMu.Unlock()

	return startMin + so, endMin + eo
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

	// technique 5 / T9: NXDOMAIN/miss chaff — a word-like random label under a
	// PUBLIC/list-or-corpus parent, never under the real domain we just replayed
	// (that would advertise the real parent to its authoritative NS). A miss is a
	// lone lookup, so it skips the dual-stack/cluster fan-out below.
	if e.rndPct(e.cfg.MissChaffPct) {
		parent := e.missChaffParent()
		if parent == "" {
			return
		}

		e.resolveOne(ctx, decoyQuery{name: e.randLabel() + "." + parent, qtype: dns.TypeA})

		return
	}

	if q.replay && e.cfg.ReplayMutate && e.rnd.Intn(2) == 0 { //nolint:mnd // 0.5 mutation probability
		q = e.mutate(q) // technique 2: never a byte-identical echo
	}

	queries := []decoyQuery{q}

	switch {
	case e.rndPct(e.cfg.ClusterPct):
		queries = e.clusterOf(q) // technique 6: small related burst
	default:
		queries = e.maybeDualStack(q) // T12: browser-style A+AAAA pair
	}

	for _, cq := range queries {
		e.resolveOne(ctx, cq)
	}
}

// missChaffParent draws the parent domain for a miss-chaff query. It prefers the
// PUBLIC Tranco list (popular domains everyone queries, so an NXDOMAIN under them
// leaks nothing), falling back to the persistent corpus only if the list is
// unseeded. Never the just-replayed real domain. "" when neither has anything.
func (e *Engine) missChaffParent() string {
	if e.source == nil {
		return ""
	}

	if d, err := e.source.SampleList(); err == nil && d != "" {
		return d
	}

	if d, err := e.source.SampleCorpus(); err == nil && d != "" {
		return d
	}

	return ""
}

// maybeDualStack (T12) mirrors how a browser resolves a hostname: with probability
// DualStackPct it emits both the A and AAAA record for an A/AAAA query. Non-address
// qtypes (HTTPS/SVCB) are left alone.
func (e *Engine) maybeDualStack(q decoyQuery) []decoyQuery {
	if q.qtype != dns.TypeA && q.qtype != dns.TypeAAAA {
		return []decoyQuery{q}
	}

	if !e.rndPct(e.cfg.DualStackPct) {
		return []decoyQuery{q}
	}

	sib := q
	if q.qtype == dns.TypeA {
		sib.qtype = dns.TypeAAAA
	} else {
		sib.qtype = dns.TypeA
	}

	return []decoyQuery{q, sib}
}

// resolveOne builds and resolves a single decoy request, stamping the sampled
// real fingerprint (technique 3) and split-upstream client identity (technique
// 4). Every request is Bypass + Decoy.
func (e *Engine) resolveOne(ctx context.Context, q decoyQuery) {
	// T6 safety net: a domain the box would itself BLOCK must never egress as
	// chaff. Source sampling already filters, but companions, dual-stack siblings
	// and miss-chaff parents don't go through that filter — guard the single
	// egress chokepoint. Exact-match only (same ceiling as IsBlockedDomain).
	if e.isBlockedDecoy(q.name) {
		return
	}

	// T5 shadow-TTL: don't re-egress the same (name,qtype) faster than its own
	// cached TTL — otherwise "same name reappears before its TTL" is a decoy tell.
	key := q.name + "/" + dns.Type(q.qtype).String()
	if e.ttlSuppressed(key) {
		return
	}

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

	resp, err := e.resolve(ctx, req)
	if err != nil {
		e.logger.WithError(err).Debug("decoy query failed")
	}

	e.noteTTL(key, resp)

	decoyQueriesTotal.Inc()
}

// isBlockedDecoy reports whether name is on the active denylist (exact match).
// Nil source (unit tests without a db) → never blocked.
func (e *Engine) isBlockedDecoy(name string) bool {
	if e.source == nil {
		return false
	}

	blocked, err := e.source.IsBlockedDomain(strings.TrimSuffix(name, "."))
	if err != nil {
		e.logger.WithError(err).Debug("decoy block check failed")

		return false // fail open: a check error shouldn't stall all cover traffic
	}

	return blocked
}

// ttlSuppressed reports whether key is still within a previously observed answer
// TTL, and should therefore not be re-emitted yet.
func (e *Engine) ttlSuppressed(key string) bool {
	if !e.cfg.ShadowTTL {
		return false
	}

	e.ttlMu.Lock()
	until, ok := e.ttlSuppress[key]
	e.ttlMu.Unlock()

	return ok && e.now().Before(until)
}

// noteTTL records the suppression window for key from the decoy's own response:
// the min answer TTL (or a short negative-cache stand-in when there's no answer).
// Opportunistically sweeps expired entries so the map can't grow unbounded.
func (e *Engine) noteTTL(key string, resp *model.Response) {
	if !e.cfg.ShadowTTL {
		return
	}

	ttl := respTTL(resp)

	now := e.now()

	e.ttlMu.Lock()
	e.ttlSuppress[key] = now.Add(time.Duration(ttl) * time.Second)

	if len(e.ttlSuppress) > ttlSuppressSweep {
		for k, until := range e.ttlSuppress {
			if now.After(until) {
				delete(e.ttlSuppress, k)
			}
		}
	}
	e.ttlMu.Unlock()
}

// respTTL returns the smallest answer-record TTL in resp, or ttlFallbackSecs when
// there is no answer (NXDOMAIN / empty) so a miss still suppresses briefly.
func respTTL(resp *model.Response) uint32 {
	if resp == nil || resp.Res == nil || len(resp.Res.Answer) == 0 {
		return ttlFallbackSecs
	}

	minTTL := resp.Res.Answer[0].Header().Ttl
	for _, rr := range resp.Res.Answer[1:] {
		if t := rr.Header().Ttl; t < minTTL {
			minTTL = t
		}
	}

	return minTTL
}

// nextQuery weighted-chooses one of three sources (T3) and returns a domain +
// qtype. The user's OWN domains dominate the public list by design:
//
//	ReplayWeight : CorpusWeight : ListWeight  =  10 : 5 : 1  (defaults)
//
// so ~94% of source draws are the household's real domains (recent 7-day replay
// or all-time corpus) and ~6% the public Tranco list — the list is seasoning that
// keeps rare/never-visited-again real domains from standing out, not the bulk.
// Any source that comes up empty (cold start, unseeded list, empty corpus) falls
// through to the list, so early on it is effectively 100% list.
func (e *Engine) nextQuery() decoyQuery {
	switch e.chooseSource() {
	case srcReplay:
		if q, err := e.source.SampleRecentReal(1); err != nil {
			e.logger.WithError(err).Debug("replay sample failed")
		} else if len(q) > 0 {
			return decoyQuery{name: q[0].Name, qtype: qtypeFromString(q[0].Qtype), replay: true}
		}
	case srcCorpus:
		if d, err := e.source.SampleCorpus(); err != nil {
			e.logger.WithError(err).Debug("corpus sample failed")
		} else if d != "" {
			return decoyQuery{name: d, qtype: e.realQtype()}
		}
	}

	domain, err := e.source.SampleList()
	if err != nil {
		e.logger.WithError(err).Debug("list sample failed")

		return decoyQuery{}
	}

	return decoyQuery{name: domain, qtype: e.realQtype()}
}

// chooseSource picks a decoy source by weight (ReplayWeight:CorpusWeight:ListWeight).
// All-zero weights (validation forbids it when enabled) degenerate to the list.
func (e *Engine) chooseSource() int {
	total := int(e.cfg.ReplayWeight + e.cfg.CorpusWeight + e.cfg.ListWeight)
	if total == 0 {
		return srcList
	}

	r := e.rnd.Intn(total)
	if r < int(e.cfg.ReplayWeight) {
		return srcReplay
	}

	if r < int(e.cfg.ReplayWeight+e.cfg.CorpusWeight) {
		return srcCorpus
	}

	return srcList
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

// applyFingerprint (technique 3) rebuilds the wire shape of a sampled real query
// onto the decoy: qclass, 0x20 case, and a full OPT record (buffer size, DO, and
// the EDNS option codes IN THE SAMPLED ORDER — option order is the discriminating
// fingerprint signal). req.Fingerprint is stamped to match what goes on the wire
// so the decoy is logged consistently with its own egress.
func (e *Engine) applyFingerprint(req *model.Request) {
	if !e.cfg.FingerprintMatch || len(req.Req.Question) == 0 {
		return
	}

	// T13: fingerprint keyed on the decoy NAME (most-recent real fp for its eTLD+1,
	// random-real fallback) so a given decoy domain presents the SAME OPT shape
	// every time it appears — a flickering shape is itself a tell.
	fp, err := e.source.SampleFingerprintForName(req.Req.Question[0].Name)
	if err != nil {
		return // query error — leave the plain decoy
	}

	if fp.QClass != 0 && fp.QClass != dns.ClassINET {
		req.Req.Question[0].Qclass = fp.QClass
		req.Fingerprint.QClass = fp.QClass
	}

	// Match the sampled client's casing behaviour: some stacks randomize 0x20,
	// most don't. Only mix case when the sampled real query did.
	if fp.Mixed0x20 {
		req.Req.Question[0].Name = e.randomize0x20(req.Req.Question[0].Name)
		req.Fingerprint.Mixed0x20 = true
	}

	if !fp.HadEDNS0 {
		return // sampled client had no OPT — leave a bare query (part of the mix)
	}

	opt := &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT}}
	opt.SetUDPSize(fp.EDNSUDPSize)
	opt.SetDo(fp.DO)

	cookie := e.cookieFor(req.ClientIP.String())
	for _, code := range fp.EDNSOptCodes {
		if o := e.synthOption(code, cookie); o != nil {
			opt.Option = append(opt.Option, o)
		}
	}

	req.Req.Extra = append(req.Req.Extra, opt)

	req.Fingerprint.HadEDNS0 = true
	req.Fingerprint.EDNSUDPSize = fp.EDNSUDPSize
	req.Fingerprint.DO = fp.DO
	req.Fingerprint.EDNSOptCodes = fp.EDNSOptCodes
	req.Fingerprint.HasCookie = fp.HasCookie
}

// synthOption builds a plausible client-side EDNS option for a sampled wire
// option code, preserving the code (and thus the OPT's code ordering) which is
// the discriminating signal. COOKIE reuses the caller-supplied per-client cookie
// (stable across this synthetic client's queries — a fresh cookie every query is
// itself unrealistic); NSID/keepalive/padding get the empty request-side payloads
// a real client actually sends.
//
// ponytail: best-effort — ECS (subnet) and opaque/local codes return nil (skipped)
// because a faithful payload would guess or leak a client subnet; only the codes
// we can honestly reconstruct are emitted, in order. Upgrade path: mirror the real
// row's ECS prefix if it's ever worth reproducing.
func (e *Engine) synthOption(code uint16, cookie string) dns.EDNS0 {
	switch code {
	case dns.EDNS0COOKIE:
		return &dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: cookie}
	case dns.EDNS0NSID:
		return &dns.EDNS0_NSID{Code: dns.EDNS0NSID} // request-side NSID is empty
	case dns.EDNS0TCPKEEPALIVE:
		return &dns.EDNS0_TCP_KEEPALIVE{Code: dns.EDNS0TCPKEEPALIVE}
	case dns.EDNS0PADDING:
		return &dns.EDNS0_PADDING{}
	default:
		return nil
	}
}

// cookieFor returns a stable 8-byte hex EDNS client cookie for a synthetic client
// (T13): a real client keeps one cookie across its queries, so we mint one per
// pseudo-client IP and reuse it, rather than a new random cookie every decoy.
func (e *Engine) cookieFor(client string) string {
	e.cookieMu.Lock()
	defer e.cookieMu.Unlock()

	if c, ok := e.cookies[client]; ok {
		return c
	}

	b := make([]byte, 8) //nolint:mnd // client cookie is 8 bytes (RFC 7873)
	for i := range b {
		b[i] = byte(e.rnd.Intn(256)) //nolint:mnd,gosec // decoy noise, not crypto
	}

	c := hex.EncodeToString(b)
	e.cookies[client] = c

	return c
}

// mutate returns a variant of a replayed query that is never byte-identical to
// the real echo: qtype flip, subdomain prepend, or 0x20 case. (T12: the old CHAOS
// class branch is gone — no real resolver emits CH-class A/AAAA, so it was a free
// decoy label for an observer.)
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
	default: // 0x20 mixed-case encoding
		q.name = e.randomize0x20(q.name)
	}

	return q
}

// maybeCompanion (technique 6, reactive) is the PRIMARY companion source: when a
// real query resolves, with probability CompanionPct it fires a browse-style
// cluster derived from that domain, after randomized sub-resource delays. The
// timer-driven clusterOf (ClusterPct) stays as a low-probability secondary for
// the quiet/no-live-tap path. Runs the burst in its own goroutine so the tap
// loop is never blocked by the inter-companion sleeps.
func (e *Engine) maybeCompanion(ctx context.Context, domain string) {
	if !e.rndPct(e.cfg.CompanionPct) {
		return
	}

	companions := e.companionsFor(domain)
	if len(companions) == 0 {
		return
	}

	go e.emitBurst(ctx, companions)
}

// emitBurst resolves each companion after a randomized, bounded delay so the
// cluster arrives spread out like a page's sub-resource lookups.
func (e *Engine) emitBurst(ctx context.Context, qs []decoyQuery) {
	for _, q := range qs {
		select {
		case <-ctx.Done():
			return
		case <-time.After(e.companionDelay()):
		}

		e.resolveOne(ctx, q)
	}
}

// companionDelay returns a randomized inter-companion delay in
// [companionDelayMinMs, companionDelayMinMs+companionDelaySpreadMs) ms.
func (e *Engine) companionDelay() time.Duration {
	ms := companionDelayMinMs + e.rnd.Intn(companionDelaySpreadMs)

	return time.Duration(ms) * time.Millisecond
}

// companionsFor derives a realistic browse cluster from a real domain: the
// first-party pair www.<eTLD+1> + base AAAA (always present) plus a random subset
// of the CDN/analytics/font/tag companion pool, in RANDOMIZED membership, count
// and ORDER (T8) so a burst carries no fixed template signature. Capped to 2..4.
func (e *Engine) companionsFor(domain string) []decoyQuery {
	base := strings.TrimSuffix(domain, ".")
	if base == "" {
		return nil
	}

	reg, err := publicsuffix.EffectiveTLDPlusOne(base)
	if err != nil || reg == "" {
		reg = base // unlisted/odd suffix — fall back to the raw name
	}

	// Follow-user: the real query already emitted the main domain (its A and
	// AAAA), so companions are ONLY sub-resource domains — never the main again.
	// www.<reg> + one guaranteed third-party sibling are always present (>=2 burst).
	keep := []decoyQuery{
		{name: "www." + reg, qtype: dns.TypeA},
		{name: clusterCompanions[e.rnd.Intn(len(clusterCompanions))], qtype: e.realQtype()},
	}

	fill := e.pickCompanions(2) //nolint:mnd // 0..2 more third-party siblings
	if d, err := e.source.SampleList(); err == nil && d != "" {
		fill = append(fill, decoyQuery{name: d, qtype: e.realQtype()})
	}

	return e.assembleBurst(nil, keep, fill) // no lead: the real query was the main
}

// clusterOf (technique 6, timer secondary) expands one query into a small related
// burst: the query itself (anchor) plus its www + AAAA siblings and random pool
// companions, in randomized count/order (T8), capped 2..4. Secondary to the
// browse-triggered maybeCompanion; kept for the quiet / no-live-tap path.
func (e *Engine) clusterOf(q decoyQuery) []decoyQuery {
	// Pure noise: this invents a page load, so the MAIN domain (q) leads and its
	// sub-resources follow (www.<main>, the main's AAAA, third-party companions).
	fill := append([]decoyQuery{
		{name: "www." + q.name, qtype: dns.TypeA},
		{name: q.name, qtype: dns.TypeAAAA},
	}, e.pickCompanions(2)...) //nolint:mnd // up to 2 pool companions

	return e.assembleBurst(&q, nil, fill)
}

// assembleBurst keeps the anchors (always present), fills up to a random 2..4 total
// with a shuffled prefix of extras, then shuffles the whole burst so ORDER is
// randomized too. Duplicate (name,qtype) pairs are dropped so a burst never carries
// the exact same query twice (and shadow-TTL can't self-suppress within it).
// assembleBurst builds a 2..4 query burst.
//   - lead (non-nil): the page's MAIN domain; stays FIRST (page-load order: HTML
//     then sub-resources) — used by the pure-noise clusterOf.
//   - keep: guaranteed members, always included (e.g. www.<reg> anchor).
//   - fill: optional sub-resources, shuffled and used to reach the burst size.
//
// keep+fill picks are shuffled together (random sub-resource order); a non-nil
// lead is prepended and stays first. Callers keep len(lead?1:0)+len(keep) <= 2.
func (e *Engine) assembleBurst(lead *decoyQuery, keep, fill []decoyQuery) []decoyQuery {
	seen := map[string]bool{}
	key := func(q decoyQuery) string { return q.name + "/" + dns.Type(q.qtype).String() }

	n := 2 + e.rnd.Intn(3) //nolint:mnd // burst size 2..4

	out := make([]decoyQuery, 0, n)
	if lead != nil {
		seen[key(*lead)] = true
		out = append(out, *lead)
	}

	body := make([]decoyQuery, 0, len(keep)+len(fill))

	for _, q := range keep { // guaranteed members
		if !seen[key(q)] {
			seen[key(q)] = true
			body = append(body, q)
		}
	}

	e.rnd.Shuffle(len(fill), func(i, j int) { fill[i], fill[j] = fill[j], fill[i] })

	for _, q := range fill { // fill up to the burst size
		if len(out)+len(body) >= n {
			break
		}

		if !seen[key(q)] {
			seen[key(q)] = true
			body = append(body, q)
		}
	}

	e.rnd.Shuffle(len(body), func(i, j int) { body[i], body[j] = body[j], body[i] })

	return append(out, body...)
}

// pickCompanions returns 0..max distinct random hosts from the companion pool, as
// A/AAAA/HTTPS decoy queries (realistic qtype mix).
func (e *Engine) pickCompanions(max int) []decoyQuery {
	idx := e.rnd.Perm(len(clusterCompanions))

	k := e.rnd.Intn(max + 1)
	if k > len(idx) {
		k = len(idx)
	}

	out := make([]decoyQuery, 0, k)
	for _, i := range idx[:k] {
		out = append(out, decoyQuery{name: clusterCompanions[i], qtype: e.realQtype()})
	}

	return out
}

// rndPct reports true with probability pct/100 (pct is validated to [0,100]).
func (e *Engine) rndPct(pct uint) bool {
	if pct == 0 {
		return false
	}

	return uint(e.rnd.Intn(100)) < pct //nolint:mnd,gosec // percent roll, not crypto
}

// labelSyllables are pronounceable fragments assembled into word-like miss-chaff
// labels (T9). Flat [a-z0-9]{6,10} labels don't look like the hostnames real
// software emits; concatenated syllables (+ an occasional digit) do.
//
//nolint:gochecknoglobals
var labelSyllables = []string{
	"ka", "lo", "mi", "ra", "ne", "to", "zu", "bi", "fa", "del", "ver", "son",
	"tri", "mon", "cor", "pix", "vex", "lum", "nex", "dar", "fen", "gil", "hov",
	"jab", "kip", "mar", "nod", "pol", "qua", "rin", "sel", "tor", "vio", "wyn",
	"cloud", "data", "edge", "node", "app", "cdn", "api", "web", "sys", "net",
}

// randLabel returns a word-like random DNS label (likely-nonexistent, for miss
// chaff): 2..3 syllables, occasionally a trailing digit, giving a realistic ~5-12
// char hostname shape rather than a flat random string.
func (e *Engine) randLabel() string {
	n := 2 + e.rnd.Intn(2) //nolint:mnd // 2..3 syllables

	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(labelSyllables[e.rnd.Intn(len(labelSyllables))])
	}

	if e.rnd.Intn(3) == 0 { //nolint:mnd // ~1/3 carry a trailing digit
		b.WriteByte(byte('0' + e.rnd.Intn(10))) //nolint:mnd // 0-9
	}

	return b.String()
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

// realQtype (T12) picks a qtype from a realistic browser/OS mix instead of the old
// A/AAAA-only coin: A and AAAA dominate, HTTPS(65) is now common, SVCB(64) rare.
// A fixed distribution — cheaper than a per-query DB sample and close enough to the
// real shape; the actual A/AAAA pairing is added separately in maybeDualStack.
func (e *Engine) realQtype() uint16 {
	switch r := e.rnd.Intn(100); { //nolint:mnd // fixed realistic qtype mix
	case r < 45:
		return dns.TypeA
	case r < 85:
		return dns.TypeAAAA
	case r < 98:
		return dns.TypeHTTPS
	default:
		return dns.TypeSVCB
	}
}

func qtypeFromString(s string) uint16 {
	if t, ok := dns.StringToType[s]; ok {
		return t
	}

	return dns.TypeA
}
