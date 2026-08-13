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
	hub     *querylog.Hub // live real-query tap; nil (non-sqlite) → historical fallback
	logger  *logrus.Entry
	rnd     *rand.Rand
	now     func() time.Time // injectable for tests

	realMu    sync.Mutex  // guards realTimes (tap goroutine writes, emit loop reads)
	realTimes []time.Time // timestamps of recent real queries within realWindow
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
			if e.withinActiveHours(e.now()) {
				e.emit(ctx)
			}

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

// applyFingerprint (technique 3) rebuilds the wire shape of a sampled real query
// onto the decoy: qclass, 0x20 case, and a full OPT record (buffer size, DO, and
// the EDNS option codes IN THE SAMPLED ORDER — option order is the discriminating
// fingerprint signal). req.Fingerprint is stamped to match what goes on the wire
// so the decoy is logged consistently with its own egress.
func (e *Engine) applyFingerprint(req *model.Request) {
	if !e.cfg.FingerprintMatch || len(req.Req.Question) == 0 {
		return
	}

	fp, err := e.source.SampleRealFingerprint()
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

	for _, code := range fp.EDNSOptCodes {
		if o := e.synthOption(code); o != nil {
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
// the discriminating signal. COOKIE gets a fresh random 8-byte client cookie so
// its length is realistic; NSID/keepalive/padding get the empty request-side
// payloads a real client actually sends.
//
// ponytail: best-effort — ECS (subnet) and opaque/local codes return nil (skipped)
// because a faithful payload would guess or leak a client subnet; only the codes
// we can honestly reconstruct are emitted, in order. Upgrade path: mirror the real
// row's ECS prefix if it's ever worth reproducing.
func (e *Engine) synthOption(code uint16) dns.EDNS0 {
	switch code {
	case dns.EDNS0COOKIE:
		b := make([]byte, 8) //nolint:mnd // client cookie is 8 bytes (RFC 7873)
		for i := range b {
			b[i] = byte(e.rnd.Intn(256)) //nolint:mnd,gosec // decoy noise, not crypto
		}

		return &dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: hex.EncodeToString(b)}
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

// companionsFor derives a realistic browse cluster from a real domain: www.<eTLD+1>,
// the AAAA of the queried base, one or two common CDN/analytics/font siblings, and
// one random noise-pool domain. Capped to a 2..4 burst (www + base AAAA always
// survive the cap, so the first-party pair is always present).
func (e *Engine) companionsFor(domain string) []decoyQuery {
	base := strings.TrimSuffix(domain, ".")
	if base == "" {
		return nil
	}

	reg, err := publicsuffix.EffectiveTLDPlusOne(base)
	if err != nil || reg == "" {
		reg = base // unlisted/odd suffix — fall back to the raw name
	}

	out := []decoyQuery{
		{name: "www." + reg, qtype: dns.TypeA},
		{name: base, qtype: dns.TypeAAAA},
	}

	for i, k := 0, 1+e.rnd.Intn(2); i < k; i++ { //nolint:mnd // 1..2 third-party siblings
		out = append(out, decoyQuery{name: clusterCompanions[e.rnd.Intn(len(clusterCompanions))], qtype: dns.TypeA})
	}

	if d, err := e.source.SampleList(); err == nil && d != "" {
		out = append(out, decoyQuery{name: d, qtype: e.randListQtype()})
	}

	n := 2 + e.rnd.Intn(3) //nolint:mnd // burst size 2..4, capped
	if n > len(out) {
		n = len(out)
	}

	return out[:n]
}

// clusterOf (technique 6, timer secondary) expands one query into a small capped
// burst of related lookups: the domain, its www + AAAA, and maybe a common
// third-party companion. Superseded as the primary companion source by
// maybeCompanion (browse-triggered); kept for the quiet / no-live-tap path.
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
