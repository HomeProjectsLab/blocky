package decoy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"slices"
	"sort"
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
	// shadowTTLCapSecs caps how long a (name,qtype) is shadow-TTL suppressed. The
	// anti-fingerprint tell is a name RE-APPEARING sooner than a real client would
	// re-resolve it — and real clients (browsers pin ~60s regardless of the
	// authoritative TTL) re-query on their own cache expiry, NOT the upstream TTL.
	// Suppressing for the full authoritative TTL (routinely 300-3600s+, sometimes a
	// day) far exceeds that and starves the compensating-cover volume: the engine
	// wants to emit at targetCurve QPM but can only re-use its small decoy-name pool
	// once per full TTL, capping egress at ~pool/TTL — orders of magnitude below
	// target. Capping at a client-cache-sized window keeps the tell covered while
	// letting cover actually reach its rate.
	shadowTTLCapSecs = 60
	// heartbeatInterval is the fixed-ish cadence (#3) of connectivity/NTP chatter
	// fired by heartbeatLoop; jittered ± heartbeatJitterMs so it isn't a clean tick.
	heartbeatInterval = 55 * time.Second
	heartbeatJitterMs = 20000
)

// Per-client persona attribution (#6) + adaptive back-off (#7) tuning.
const (
	// personaPoolSize bounds the rotating pool of sampled real-client personas.
	// A household has few clients, so the pool fills fast and then stops sampling.
	personaPoolSize = 8
	// Adaptive back-off: over the last backoffWindow decoy resolve outcomes, if the
	// error rate exceeds backoffErrThreshold the rate multiplier is cut by
	// backoffDecay (floored at backoffMin); otherwise it recovers by backoffRecover.
	backoffWindow       = 20
	backoffMinSamples   = 10
	backoffErrThreshold = 0.30
	backoffDecay        = 0.80
	backoffRecover      = 0.05
	backoffMin          = 0.15
)

// decoy source selectors (T3 three-way weighted mix in nextQuery).
const (
	srcReplay = iota // recent real-query replay pool (7-day window)
	srcCorpus        // persistent all-time visited-domains corpus
	srcList          // public Tranco static list
)

// Provenance labels stamped on decoyQuery.source and persisted to
// log_entries.decoy_source so the Noise dashboard can break decoys down by what
// produced them. Session-walk and revisit picks are the household's own corpus
// domains, so they carry provReplay/provCorpus/provList from their draw.
const (
	provReplay    = "replay"
	provCorpus    = "corpus"
	provList      = "list"
	provCohort    = "cohort"
	provCompanion = "companion"
	provChatter   = "chatter"
	provMiss      = "miss"
	provFail      = "fail"
	provBeacon    = "beacon" // iot vendor-telemetry beacon (device-class shaping)
	provServer    = "server" // server registry/update/monitoring lookup
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

// deviceChatter (#3) is the non-web DNS a real device emits in the background:
// captive-portal / connectivity checks, NTP pools, and OS/app telemetry-ish
// endpoints. A share of emissions (ChatterPct) draws from here so the box's
// egress isn't 100% browsing. Blocked telemetry names are dropped at the
// resolveOne chokepoint like any other decoy — no leak.
//
//nolint:gochecknoglobals
var deviceChatter = []string{
	// connectivity / captive-portal checks
	"captive.apple.com", "connectivitycheck.gstatic.com", "clients3.google.com",
	"www.msftconnecttest.com", "www.msftncsi.com", "detectportal.firefox.com",
	"network-test.debian.org", "nmcheck.gnome.org",
	// NTP
	"pool.ntp.org", "time.apple.com", "time.windows.com", "time.google.com",
	"time.cloudflare.com", "time.android.com",
	// OS / app telemetry-ish
	"push.apple.com", "settings-win.data.microsoft.com", "incoming.telemetry.mozilla.org",
	"app-measurement.com", "firebaseinstallations.googleapis.com", "mtalk.google.com",
}

// heartbeatHosts are the subset of device chatter a real host repeats on a
// fixed-ish TIMER (connectivity + NTP), fired by heartbeatLoop rather than the
// Poisson emit path — periodicity itself is part of their realism.
//
//nolint:gochecknoglobals
var heartbeatHosts = []string{
	"captive.apple.com", "connectivitycheck.gstatic.com", "www.msftconnecttest.com",
	"detectportal.firefox.com", "pool.ntp.org", "time.google.com",
}

// vendorTelemetry (device-class IoT shaping) is the embedded set of solar/inverter/
// smart-home telemetry endpoints, grouped by vendor FAMILY. IoT-classed clients
// beacon to fixed vendor clouds — they don't browse — so an iot-attributed decoy
// draws a low-diversity BEACON from here instead of a page-load cohort. To OBSCURE
// the real fleet, beaconFamily also draws from families the site may not run
// (phantom/other-vendor), so a sniffer can't read the true vendor or device count
// off these low-entropy beacons. Blocked names still drop at isBlockedDecoy.
//
//nolint:gochecknoglobals
var vendorTelemetry = map[string][]string{
	// solar / inverter / storage
	"solaredge": {"monitoring.solaredge.com", "monitoringapi.solaredge.com"},
	"fronius":   {"www.solarweb.com", "fronius.solarweb.com"},
	"huawei":    {"eu5.fusionsolar.huawei.com", "intl.fusionsolar.huawei.com"},
	"sma":       {"ennexos.sunnyportal.com", "www.sunnyportal.com"},
	"enphase":   {"enlighten.enphaseenergy.com", "api.enphaseenergy.com"},
	"tesla":     {"owner-api.teslamotors.com", "energy.tesla.com"},
	"growatt":   {"server.growatt.com", "openapi.growatt.com"},
	"victron":   {"vrm.victronenergy.com", "mqtt.victronenergy.com"},
	"goodwe":    {"www.semsportal.com", "eu.semsportal.com"},
	// generic smart-home / IoT clouds
	"tuya":   {"m1.tuyaeu.com", "a1.tuyaeu.com"},
	"sonos":  {"devices.sonos.com", "update-firmware.sonos.com"},
	"ring":   {"api.ring.com", "fw.ring.com"},
	"shelly": {"shelly-api-eu.shelly.cloud", "api.shelly.cloud"},
	"hue":    {"ws.meethue.com", "data.meethue.com"},
	"ecobee": {"api.ecobee.com", "eapi.ecobee.com"},
	"nest":   {"device-provisioning.googleapis.com", "nexusapi-eu1.camera.home.nest.com"},
}

// serverTelemetry (device-class server shaping) is the embedded set of registry/
// package/OS-mirror/update/monitoring endpoints a SERVER-classed client hits. A
// server-attributed decoy draws one of these instead of a human page load.
//
//nolint:gochecknoglobals
var serverTelemetry = []string{
	"registry-1.docker.io", "auth.docker.io", "index.docker.io", "ghcr.io", "quay.io", "gcr.io",
	"deb.debian.org", "security.debian.org", "archive.ubuntu.com", "security.ubuntu.com",
	"mirror.centos.org", "download.fedoraproject.org",
	"pypi.org", "files.pythonhosted.org", "registry.npmjs.org", "crates.io",
	"proxy.golang.org", "sum.golang.org", "objects.githubusercontent.com",
	"grafana.com", "prometheus.io", "acme-v02.api.letsencrypt.org",
}

// deadTLDs are real, resolvable TLDs under which a RANDOM second-level label is
// almost certainly unregistered — a plausible-looking NXDOMAIN (#4 fail chaff),
// unlike .invalid which no real client ever queries.
//
//nolint:gochecknoglobals
var deadTLDs = []string{"com", "net", "org", "info", "xyz", "io", "co"}

// ResolveFunc runs a request through the server's resolver chain (same path
// real queries take, so decoys follow the active group strategy/recursion).
type ResolveFunc func(ctx context.Context, req *model.Request) (*model.Response, error)

// Source is the read side of the decoy engine's query-log store. The production
// implementation is *querylog.DecoySource; tests inject a mock. Defined here (at
// the consumer) only so the structural-emission mechanics have deterministic
// unit coverage without a live sqlite fixture.
type Source interface {
	SeedIfEmpty(io.Reader) (int, error)
	HourlyRealCounts() ([24]int64, error)
	SampleList() (string, error)
	SampleCorpus() (string, error)
	SampleRecentReal(limit int) ([]querylog.RealQuery, error)
	SampleFingerprintForName(name string) (querylog.FpSample, error)
	IsBlockedDomain(domain string) (bool, error)
	SampleCohort() ([]querylog.CohortMember, error)
	NextInSession(primaryDomain string) (string, error)
	SessionSeed() (string, error)
	RevisitInterval(domain string) (time.Duration, bool)
	SampleClient() (querylog.ClientPersona, error)
	ClientClass(client string) (string, error)
}

// Engine emits background noise queries at a randomized rate, mixing replayed
// real queries with entries from the static list. Every emitted request is
// marked Bypass (skip cache) and Decoy (excluded from dashboards).
type Engine struct {
	cfg     config.DecoyConfig
	source  Source
	resolve ResolveFunc
	hub     *querylog.Hub // live real-query tap; nil (non-sqlite) → historical fallback
	logger  *logrus.Entry
	rnd     *rand.Rand
	now     func() time.Time // injectable for tests

	realMu    sync.Mutex  // guards realTimes (tap goroutine writes, emit loop reads)
	realTimes []time.Time // timestamps of recent real queries within realWindow

	sessMu        sync.Mutex // guards session-walk state (#1)
	sessionDomain string     // current session's eTLD+1 anchor ("" = start fresh)
	sessionSteps  int        // hops taken in the current session (capped by sessionCap)

	dueMu  sync.Mutex           // guards dueMap (#5 revisit cadence)
	dueMap map[string]time.Time // domain -> next-due emission time (bounded by revisitMapCap)

	ttlMu       sync.Mutex           // guards ttlSuppress (T5 shadow-TTL)
	ttlSuppress map[string]time.Time // (name/qtype) -> earliest re-egress time

	cookieMu sync.Mutex        // guards cookies (T13 per-client stable EDNS cookie)
	cookies  map[string]string // pseudo-client IP -> stable hex cookie

	edgeMu   sync.Mutex // guards the per-day active-hours edge jitter (T10)
	edgeDay  string     // yyyy-mm-dd the current edge offsets were drawn for
	startOff int        // today's start-edge jitter, minutes
	endOff   int        // today's end-edge jitter, minutes

	personaMu sync.Mutex               // guards personas (#6 per-client attribution)
	personas  []querylog.ClientPersona // small rotating pool of sampled real clients

	backoffMu sync.Mutex // guards adaptive-backoff state (#7)
	outcomes  []bool     // ring of recent decoy resolve outcomes (true = failed)
	backoff   float64    // current decoy-rate multiplier in [backoffMin, 1]
}

// maxConcurrentEmits bounds how many timer emissions run at once (see Run). emit
// does per-emission full-table sqlite sampling + a blocking upstream resolve, so
// running it inline serialized the whole engine behind that latency — the
// configured QueriesPerMinute/targetQpm was silently unreachable (~1 emit per
// DB-scan). A small pool overlaps that I/O while capping load on a Pi-class box.
//
// ponytail: fixed pool sized for I/O-wait overlap, not CPU. The real ceiling is
// per-emit full-table scan cost (SessionSeed/NextInSession window scans,
// SampleCohort ORDER BY RANDOM()); if 8× headroom isn't enough, cache those
// derivations (short-TTL) so emit() stops scanning the log per emission.
const maxConcurrentEmits = 8

// lockedSource makes a *rand.Rand safe for concurrent use (the stdlib's own
// global rand does exactly this). The engine's rnd is now read from the emit
// worker pool, the companion/cohort burst goroutines and the heartbeat loop at
// once; a bare math/rand.Source is not goroutine-safe.
type lockedSource struct {
	mu  sync.Mutex
	src rand.Source64
}

func (s *lockedSource) Int63() int64    { s.mu.Lock(); defer s.mu.Unlock(); return s.src.Int63() }
func (s *lockedSource) Uint64() uint64  { s.mu.Lock(); defer s.mu.Unlock(); return s.src.Uint64() }
func (s *lockedSource) Seed(seed int64) { s.mu.Lock(); defer s.mu.Unlock(); s.src.Seed(seed) }

func NewEngine(cfg config.DecoyConfig, source Source, resolve ResolveFunc) *Engine {
	return &Engine{
		cfg:     cfg,
		source:  source,
		resolve: resolve,
		logger:  log.PrefixedLog("decoy"),
		//nolint:gosec // noise timing, not crypto; locked for concurrent emit workers
		rnd:         rand.New(&lockedSource{src: rand.NewSource(time.Now().UnixNano()).(rand.Source64)}),
		now:         time.Now,
		ttlSuppress: map[string]time.Time{},
		cookies:     map[string]string{},
		dueMap:      map[string]time.Time{},
		backoff:     1,
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

	if e.cfg.ChatterPct > 0 {
		go e.heartbeatLoop(ctx) // #3: connectivity/NTP heartbeats on a fixed-ish timer
	}

	timer := time.NewTimer(e.nextInterval())
	defer timer.Stop()

	// Dispatch each emission to a bounded worker pool instead of running it inline:
	// emit() blocks on full-table DB sampling + an upstream resolve, and inline that
	// serialized the engine behind that latency (the configured rate was unreachable
	// — decoys UNDER-generated). The scheduler now keeps firing at effectiveQPM while
	// up to maxConcurrentEmits emissions run in parallel. When the pool is saturated
	// the tick is dropped: natural backpressure so a slow box can't pile unbounded
	// scans onto the DB.
	sem := make(chan struct{}, maxConcurrentEmits)

	// Log-state tracking (single goroutine; no locking needed). active-hours flips
	// and an hourly effective-rate snapshot are the human-wanted incident signals;
	// dropped emissions are rate-limited so a slow box can't turn them into a flood.
	active := e.withinActiveHours(e.now())
	rateHour := -1

	var dropped int

	var lastDropLog time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			now := e.now()

			if a := e.withinActiveHours(now); a != active {
				active = a
				if a {
					e.logger.WithField("effective_qpm", e.effectiveQPM()).Info("decoy entered active hours")
				} else {
					e.logger.WithField("floor_qpm", e.cfg.OffHoursFloorQPM).
						Info("decoy left active hours; dropped to off-hours floor")
				}
			}

			// Hourly snapshot of the effective rate vs the persona target and the
			// live real rate — shows rate/target drift without per-interval spam.
			if h := now.Hour(); h != rateHour {
				rateHour = h
				e.logger.WithField("effective_qpm", e.effectiveQPM()).
					WithField("target_qpm", e.targetCurve(now)).
					WithField("real_qpm", e.recentRealQPM()).
					WithField("backoff", e.backoffFactor()).
					Info("decoy effective rate")
			}

			// T10: never gate fully to zero — emit every tick. Outside active
			// hours effectiveQPM (via nextInterval) collapses to the low
			// always-on floor rather than stopping, so an observer can't read the
			// window edges as a clean on/off step.
			select {
			case sem <- struct{}{}:
				go func() {
					defer func() { <-sem }()
					e.emit(ctx)
				}()
			default:
				// pool saturated (box can't keep up) — drop this emission.
				// WARN rate-limited to one line per 30s with the drop count.
				dropped++
				if time.Since(lastDropLog) >= 30*time.Second {
					e.logger.WithField("dropped", dropped).
						Warn("decoy emission pool saturated; dropping emissions (box can't keep up)")

					dropped = 0
					lastDropLog = time.Now()
				}
			}

			timer.Reset(e.nextInterval())
		}
	}
}

// nextInterval draws an exponential inter-arrival time (Poisson process) for the
// current effective rate.
func (e *Engine) nextInterval() time.Duration {
	qpm := e.effectiveQPM() * e.backoffFactor() // #7: throttle under upstream strain
	if qpm < 0.01 {                             //nolint:mnd // floor so meanSeconds never explodes
		qpm = 0.01
	}

	meanSeconds := 60.0 / qpm

	return time.Duration(e.rnd.ExpFloat64() * meanSeconds * float64(time.Second))
}

// effectiveQPM is the decoy rate for the next interval.
//
// With PersonaCover on (#8, the default) it is COMPENSATING cover: the decoy
// rate fills the gap between a household diurnal TARGET curve and the live real
// rate — decoy_rate = max(0, targetCurve(t) − recentRealQPM) — so TOTAL egress
// tracks the persona curve regardless of real activity. This hides the activity
// LEVEL, not just which queries are real, without ever delaying a real query.
// Residual: real usage above the peak ceiling still spikes total egress (see the
// personaCover field comment) — the curve hides level only up to the peak.
//
// With PersonaCover off it falls back to the reactive/diurnal path: ReactiveVolume
// tracks recent real QPS ± a masking term, else the base scaled by the historical
// diurnal shape. That path leaks the activity level (the decoy rate rises with
// real QPS) — the reason PersonaCover supersedes it by default.
func (e *Engine) effectiveQPM() float64 {
	// T10: outside active hours we don't stop — we drop to a low always-on floor.
	if !e.withinActiveHours(e.now()) {
		return e.cfg.OffHoursFloorQPM
	}

	if e.cfg.PersonaCover {
		cover := e.targetCurve(e.now()) - e.recentRealQPM()
		if cover < 0 {
			cover = 0 // real usage already meets/exceeds the target — no cover needed
		}

		return cover
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

// personaShape is a generic household activity shape (relative, 0..1) indexed by
// LOCAL hour-of-day: quiet pre-dawn, a morning rise, a sustained daytime plateau,
// and an evening peak. targetCurve scales it between the configured trough/peak.
// A fixed table (not learned) is the whole point of #8: the persona is a STABLE
// generic household, so the box's own diurnal signature can't be read off it.
//
//nolint:gochecknoglobals
var personaShape = [24]float64{
	0.15, 0.10, 0.07, 0.05, 0.05, 0.10, 0.25, 0.50, 0.80, 0.85, 0.75, 0.70, // 00-11
	0.72, 0.70, 0.68, 0.70, 0.75, 0.85, 0.95, 1.00, 0.95, 0.80, 0.50, 0.25, // 12-23
}

// enterpriseShape is the office-diurnal activity shape: a sharp 07-09 ramp, a
// sustained daytime plateau, an evening drop — over a high overnight FLOOR (never
// near 0) that stands in for 24/7 servers + always-on IoT beaconing. Selected when
// PersonaProfile is "enterprise"; scaled between the (higher) enterprise curve's
// trough and peak by targetCurve.
//
//nolint:gochecknoglobals
var enterpriseShape = [24]float64{
	0.40, 0.38, 0.37, 0.37, 0.38, 0.42, 0.55, 0.80, 0.95, 1.00, 0.98, 0.97, // 00-11
	0.95, 0.97, 0.98, 0.95, 0.90, 0.80, 0.60, 0.50, 0.45, 0.43, 0.42, 0.41, // 12-23
}

// targetCurve is the persona's target TOTAL egress (queries/min) for t's local
// hour, interpolated between the resolved trough/peak (EffectivePersonaCurve —
// preset + explicit overrides) by the profile's activity shape.
func (e *Engine) targetCurve(t time.Time) float64 {
	peak, trough := e.cfg.EffectivePersonaCurve()
	if peak < trough {
		peak = trough
	}

	shape := personaShape
	if e.cfg.PersonaProfile == "enterprise" {
		shape = enterpriseShape
	}

	return trough + shape[t.Hour()]*(peak-trough)
}

// recentRealQPM is the live real-query rate (queries/min) over realWindow.
func (e *Engine) recentRealQPM() float64 {
	return float64(e.recentRealCount()) / realWindow.Seconds() * 60.0
}

// reactiveQPM returns (live real-query count in the window, target decoy QPM).
// The target is the live real rate plus a per-interval ± random(reactiveJitterQPM)
// queries/min term — an additive mask so the decoy rate is never exactly the real
// rate — floored at minReactiveQPM.
func (e *Engine) reactiveQPM() (int, float64) {
	n := e.recentRealCount()

	realQPM := float64(n) / realWindow.Seconds() * 60.0

	jitter := float64(e.rnd.Intn(2*reactiveJitterQPM+1) - reactiveJitterQPM)

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
		e.startOff = e.rnd.Intn(2*j+1) - j
		e.endOff = e.rnd.Intn(2*j+1) - j
	}
	so, eo := e.startOff, e.endOff
	e.edgeMu.Unlock()

	return startMin + so, endMin + eo
}

// decoyQuery is one synthetic lookup to emit. qclass 0 means default IN.
type decoyQuery struct {
	name         string
	qtype        uint16
	qclass       uint16
	source       string // provenance label (prov* consts) -> log_entries.decoy_source
	replay       bool   // sourced from the real-query replay pool
	allowBlocked bool   // recorded-cohort member: egress even if the box would block it (shadow-completion parity)
	delayMs      int    // recorded page-load offset from the cohort's first member (cohort emission only)
	// persona, when non-nil, forces this decoy's client attribution (device-class
	// routing already chose the client + its class); nil lets resolveOne pick one.
	persona *querylog.ClientPersona
}

// sessionCap bounds a synthetic session's length: after this many topical hops
// we force a fresh reseed, so a decoy "session" never walks an implausibly long
// chain an observer could distinguish from real human browsing.
const sessionCap = 8

// emit builds the decoy queries for one emission and resolves each through the
// chain. The emission mix, in order:
//   - T9 miss chaff (lone NXDOMAIN-ish lookup)     — MissChaffPct
//   - 7G recorded cohort (real page-load texture)  — CohortPct  (structural)
//   - session-coherent synthetic single/cluster    — remainder  (structural)
//
// The structural share walks plausible SESSIONS (#1) and honours REVISIT cadence
// (#5); persona cover (#8) shapes the RATE at which emit is called, not its
// content. Cohorts are the recorded texture; sessions are the sequence.
func (e *Engine) emit(ctx context.Context) {
	// Device-class routing: attribute this emission to a real client and SHAPE the
	// chaff to that client's class — iot beacons to vendor telemetry, servers hit
	// registries/updates — instead of human browsing. Workstation/unknown (and the
	// disabled/cold-start case) fall through to the browsing path below.
	if e.cfg.DeviceClass.Enable {
		if persona, class, ok := e.classPersona(); ok {
			switch class {
			case querylog.ClassIoT:
				e.emitBeacon(ctx, persona)

				return
			case querylog.ClassServer:
				e.emitServer(ctx, persona)

				return
			}
		}
	}

	// technique 5 / T9: NXDOMAIN/miss chaff — a word-like random label under a
	// PUBLIC/list-or-corpus parent. A miss is a lone lookup, so it skips the
	// structural cohort/cluster paths below.
	if e.rndPct(e.cfg.MissChaffPct) {
		parent := e.missChaffParent()
		if parent == "" {
			return
		}

		e.resolveOne(ctx, decoyQuery{name: e.randLabel() + "." + parent, qtype: dns.TypeA, source: provMiss})

		return
	}

	// #3: a share of emissions is device background chatter (connectivity/NTP/
	// telemetry/PTR) rather than browsing — a lone non-web lookup.
	if e.rndPct(e.cfg.ChatterPct) {
		e.resolveOne(ctx, e.chatterQuery())

		return
	}

	// #4 failure realism: a share deliberately queries a likely-NXDOMAIN name so
	// decoys don't always resolve (real traffic carries a background miss rate).
	if e.rndPct(e.cfg.FailChaffPct) {
		e.resolveOne(ctx, decoyQuery{name: e.deadName(), qtype: dns.TypeA, source: provFail})

		return
	}

	// 7G: a share of emissions replays a whole RECORDED page-load cohort with its
	// real per-member timing (incl. blocked members). SUPERSEDES the synthetic
	// clusterOf as the primary cohort source; clusterOf is the cold-start fallback
	// below when no real cohort is available.
	if e.rndPct(e.cfg.CohortPct) && e.emitCohort(ctx) {
		return
	}

	q := e.nextStructuralQuery()
	if q.name == "" {
		return
	}

	if q.replay && e.cfg.ReplayMutate && e.rnd.Intn(2) == 0 {
		q = e.mutate(q) // technique 2: never a byte-identical echo
	}

	queries := []decoyQuery{q}

	switch {
	case e.rndPct(e.cfg.ClusterPct):
		queries = e.clusterOf(q) // technique 6: small related burst (synthetic cohort)
	default:
		queries = e.maybeDualStack(q) // T12: browser-style A+AAAA pair
	}

	for _, cq := range queries {
		e.resolveOne(ctx, cq)
	}
}

// emitCohort replays a recorded real page-load cohort as chaff: its members in
// recorded order, each fired at its own recorded page-load offset (emitBurstTimed),
// primary first. Blocked members are included and egressed (allowBlocked) — real
// cohorts are wire-complete via shadow-completion, so decoy cohorts must match.
// Returns false at cold start (no recorded cohort yet) so emit falls back to the
// synthetic path. Anchors the session walk to the cohort's real primary.
func (e *Engine) emitCohort(ctx context.Context) bool {
	if e.source == nil {
		return false
	}

	cohort, err := e.source.SampleCohort()
	if err != nil {
		e.logger.WithError(err).Debug("cohort sample failed")

		return false
	}

	if len(cohort) == 0 {
		return false // cold start
	}

	e.anchorSession(cohort[0].Domain) // the real primary re-anchors the session sequence

	qs := make([]decoyQuery, 0, len(cohort))
	for _, m := range cohort {
		qs = append(qs, decoyQuery{name: m.Domain, qtype: m.Qtype, source: provCohort, allowBlocked: m.Blocked, delayMs: m.DelayMs})
	}

	go e.emitBurstTimed(ctx, e.perturbCohort(qs))

	return true
}

// Cohort-replay perturbation. A recorded cohort is faithful real texture, but
// emitting it byte-identically every time is itself a signature. perturbCohort
// keeps the primary leading (the main document still comes first) and:
//   - jitters each sub-resource's recorded offset by a small ± amount, then the
//     caller-visible re-sort turns overlapping jitters into the small run-to-run
//     reordering that real resource scheduling already produces;
//   - with a small probability splices in ONE unrelated companion at a random
//     point in the load.
//
// So no two replays of the same cohort are identical, while the page-load shape
// (which domains, roughly when) stays real.
//
// Jitter magnitude and companion-splice share are DecoyConfig knobs
// (CohortJitterMs / CohortCompanionPct); a jitter of 0 means exact 1:1 replay.
func (e *Engine) perturbCohort(qs []decoyQuery) []decoyQuery {
	if len(qs) <= 1 {
		return qs // nothing to reorder, and no burst to hide a splice in
	}

	maxDelay := qs[len(qs)-1].delayMs

	// jitter sub-resource offsets; index 0 is the primary (delay 0) and stays put
	// so the main document still leads.
	if j := int(e.cfg.CohortJitterMs); j > 0 {
		for i := 1; i < len(qs); i++ {
			qs[i].delayMs = max(qs[i].delayMs+e.rnd.Intn(2*j+1)-j, 1)
			maxDelay = max(maxDelay, qs[i].delayMs)
		}
	}

	// splice in one unrelated companion at a random point in the load
	if e.rndPct(e.cfg.CohortCompanionPct) {
		qs = append(qs, decoyQuery{
			name:    clusterCompanions[e.rnd.Intn(len(clusterCompanions))],
			qtype:   e.realQtype(),
			source:  provCompanion,
			delayMs: 1 + e.rnd.Intn(max(maxDelay, 1)),
		})
	}

	// emission order follows the perturbed timeline; the primary (delay 0) leads.
	sort.SliceStable(qs, func(i, j int) bool { return qs[i].delayMs < qs[j].delayMs })

	return qs
}

// emitBurstTimed resolves a recorded cohort spread across its ORIGINAL page-load
// timeline: each member waits until its recorded delayMs offset from the first.
// Members are already in ascending-delay order (primary first, delayMs 0).
func (e *Engine) emitBurstTimed(ctx context.Context, qs []decoyQuery) {
	prev := 0
	for _, q := range qs {
		wait := max(q.delayMs-prev,
			// out-of-order/clock-skew guard
			0)

		prev = q.delayMs

		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(wait) * time.Millisecond):
		}

		e.resolveOne(ctx, q)
	}
}

// nextStructuralQuery picks the domain for one synthetic (non-cohort) emission.
// With SessionCoherence it walks a plausible session chain; otherwise (or when
// the walk has no successor) it falls back to a revisit-biased source pick.
func (e *Engine) nextStructuralQuery() decoyQuery {
	if e.cfg.SessionCoherence && e.source != nil {
		if dom := e.walkSession(); dom != "" {
			return decoyQuery{name: dom, qtype: e.realQtype(), source: provCorpus}
		}
	}

	return e.reviseOrNext()
}

// walkSession advances the current session (#1): with probability StepPct it
// steps to a topically-plausible successor of the current anchor via
// NextInSession; on no successor, a reached session cap, or a StepPct miss it
// reseeds a fresh session via SessionSeed. Returns "" only when even reseeding
// has no data (cold start) so the caller falls back to a plain source pick.
func (e *Engine) walkSession() string {
	e.sessMu.Lock()
	cur, steps := e.sessionDomain, e.sessionSteps
	e.sessMu.Unlock()

	if cur != "" && steps < sessionCap && e.rndPct(e.cfg.StepPct) {
		if next, err := e.source.NextInSession(cur); err == nil && next != "" {
			e.setSession(next, steps+1)

			return next
		}
	}

	seed, err := e.source.SessionSeed()
	if err != nil || seed == "" {
		return "" // cold start — caller falls back
	}

	e.setSession(seed, 1)

	return seed
}

// anchorSession re-points the session walk at a real domain's eTLD+1 (used when a
// recorded cohort is emitted, so the next synthetic hop follows from it). No-op
// when session coherence is off or the name has no registrable domain.
func (e *Engine) anchorSession(name string) {
	if !e.cfg.SessionCoherence {
		return
	}

	reg, err := publicsuffix.EffectiveTLDPlusOne(strings.TrimSuffix(name, "."))
	if err != nil || reg == "" {
		return
	}

	e.setSession(reg, 1)
}

func (e *Engine) setSession(domain string, steps int) {
	e.sessMu.Lock()
	e.sessionDomain = domain
	e.sessionSteps = steps
	e.sessMu.Unlock()
}

// revisitMapCap bounds the per-domain next-due map (#5) so it can't grow with the
// corpus. A few hundred tracked domains is plenty to give the frequently-revisited
// ones a human cadence; the rest flow through the normal random source picks.
const revisitMapCap = 256

// reviseOrNext returns the next synthetic domain, biased toward REVISIT cadence
// (#5): if a tracked domain is due per its learned interval, emit it now;
// otherwise take a normal weighted source pick and schedule its next due time.
func (e *Engine) reviseOrNext() decoyQuery {
	if e.cfg.RevisitCadence {
		if d := e.dueDomain(); d != "" {
			return decoyQuery{name: d, qtype: e.realQtype(), source: provCorpus}
		}
	}

	q := e.nextQuery()
	if e.cfg.RevisitCadence && q.name != "" {
		e.scheduleRevisit(q.name)
	}

	return q
}

// dueDomain returns a tracked domain whose learned revisit interval has elapsed
// (rescheduling it), or "" if none is due. Cheap linear scan of a bounded map.
func (e *Engine) dueDomain() string {
	now := e.now()

	e.dueMu.Lock()
	defer e.dueMu.Unlock()

	for d, due := range e.dueMap {
		if !now.Before(due) {
			e.scheduleRevisitLocked(d, now)

			return d
		}
	}

	return ""
}

func (e *Engine) scheduleRevisit(domain string) {
	e.dueMu.Lock()
	defer e.dueMu.Unlock()

	e.scheduleRevisitLocked(domain, e.now())
}

// scheduleRevisitLocked sets domain's next-due time from its learned revisit
// interval (± jitter). A domain with no learned cadence is not tracked (dropped),
// so it just keeps flowing through random picks. Evicts an arbitrary entry when
// the map is at cap.
func (e *Engine) scheduleRevisitLocked(domain string, now time.Time) {
	iv, ok := e.source.RevisitInterval(domain)
	if !ok {
		delete(e.dueMap, domain)

		return
	}

	jitter := 1 + (e.rnd.Float64()-0.5)*0.5 //nolint:mnd // ±25% cadence jitter
	due := now.Add(time.Duration(float64(iv) * jitter))

	if _, tracked := e.dueMap[domain]; !tracked && len(e.dueMap) >= revisitMapCap {
		for k := range e.dueMap { // evict one arbitrary entry to stay bounded
			delete(e.dueMap, k)

			break
		}
	}

	e.dueMap[domain] = due
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
	//
	// Exception: recorded-cohort members (allowBlocked) DO egress even when
	// blocked — real page-load cohorts are made wire-complete by shadow-completion
	// in the resolver, so a decoy cohort that dropped its blocked members would be
	// distinguishable from a real one. 7G deliberately mirrors the real cohort.
	if !q.allowBlocked && e.isBlockedDecoy(q.name) {
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

	// #4 transport diversity: a share of decoys present as TCP. See the tcpPct
	// limitation on Protocol — this stamps the request (client-facing log/rate
	// shape); the actual blocky→upstream transport is upstream-config-global.
	proto := model.RequestProtocolUDP
	if e.rndPct(e.cfg.TCPPct) {
		proto = model.RequestProtocolTCP
	}

	// #6 per-client persona attribution: stamp a sampled real client's IP (and, with
	// FingerprintMatch, that client's OPT shape) so chaff is attributed to plausible
	// real clients and each client's wire profile stays consistent. Falls back to
	// the synthetic split-upstream identity + name-keyed fingerprint when disabled
	// or at cold start.
	persona, attributed := e.pickPersona()
	if q.persona != nil { // device-class routing already chose the client
		persona, attributed = *q.persona, true
	}

	ip := e.clientIP()
	if attributed {
		if p := net.ParseIP(persona.IP); p != nil {
			ip = p
		} else {
			attributed = false
		}
	}

	req := &model.Request{
		ClientIP:    ip,
		Req:         msg,
		Protocol:    proto,
		RequestTS:   e.now(),
		Bypass:      true,
		Decoy:       true,
		DecoySource: q.source,
	}

	if attributed && e.cfg.FingerprintMatch {
		e.stampFingerprint(req, persona.Fp)
	} else {
		e.applyFingerprint(req)
	}

	resp, err := e.resolve(ctx, req)
	if err != nil {
		e.logger.WithError(err).Debug("decoy query failed")

		// #4: real stubs retry on timeout — occasionally re-issue the failed decoy.
		// ponytail: single immediate retry, no separate backoff; the request object
		// is reused (a retry is byte-identical to the original by design).
		if e.rnd.Intn(2) == 0 {
			resp, err = e.resolve(ctx, req)
		}
	}

	e.noteOutcome(err != nil) // #7 feed the adaptive-backoff window
	e.noteTTL(key, resp)

	decoyQueriesTotal.Inc()

	// Sampled hot-path trace (Debug, off by default; 1-in-emitDebugSample). Never
	// the domain — replay/corpus sources derive from real visited domains, so the
	// name stays out of the log; source label + qtype + outcome are enough.
	if e.rnd.Intn(emitDebugSample) == 0 {
		e.logger.WithField("source", q.source).
			WithField("qtype", dns.Type(q.qtype).String()).
			WithField("failed", err != nil).
			Debug("decoy emitted")
	}
}

// emitDebugSample is the 1-in-N sampling rate for the per-emit Debug trace: even
// at Debug the decoy path is hot, so it is sampled to stay a trickle, not a flood.
const emitDebugSample = 64

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
	if ttl > shadowTTLCapSecs {
		ttl = shadowTTLCapSecs // don't outlast a real client's DNS cache; see const
	}

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
			return decoyQuery{name: q[0].Name, qtype: qtypeFromString(q[0].Qtype), source: provReplay, replay: true}
		}
	case srcCorpus:
		if d, err := e.source.SampleCorpus(); err != nil {
			e.logger.WithError(err).Debug("corpus sample failed")
		} else if d != "" {
			return decoyQuery{name: d, qtype: e.realQtype(), source: provCorpus}
		}
	}

	domain, err := e.source.SampleList()
	if err != nil {
		e.logger.WithError(err).Debug("list sample failed")

		return decoyQuery{}
	}

	return decoyQuery{name: domain, qtype: e.realQtype(), source: provList}
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

// chatterQuery (#3) returns one device-background lookup: mostly a name from the
// embedded deviceChatter set, ~20% a PTR reverse lookup of a random RFC1918
// address (real hosts reverse-resolve LAN peers and their own gateway).
func (e *Engine) chatterQuery() decoyQuery {
	if e.rnd.Intn(5) == 0 {
		return decoyQuery{name: e.randPTRName(), qtype: dns.TypePTR, source: provChatter}
	}

	return decoyQuery{name: deviceChatter[e.rnd.Intn(len(deviceChatter))], qtype: e.realQtype(), source: provChatter}
}

// randPTRName builds an in-addr.arpa PTR name for a random 192.168.x.y host (the
// common home range) — a plausible LAN reverse lookup.
func (e *Engine) randPTRName() string {
	return fmt.Sprintf("%d.%d.168.192.in-addr.arpa.", e.rnd.Intn(256), e.rnd.Intn(256)) //nolint:mnd // octet range
}

// deadName (#4) returns a likely-NXDOMAIN name: a word-like random label under a
// real TLD, almost certainly unregistered.
func (e *Engine) deadName() string {
	return e.randLabel() + "." + deadTLDs[e.rnd.Intn(len(deadTLDs))]
}

// heartbeatLoop (#3) fires a connectivity/NTP lookup on a fixed-ish timer
// (heartbeatInterval ± jitter) rather than the Poisson emit path — the periodicity
// is part of these checks' realism. Runs until ctx is cancelled.
func (e *Engine) heartbeatLoop(ctx context.Context) {
	for {
		d := heartbeatInterval + time.Duration(e.rnd.Intn(2*heartbeatJitterMs)-heartbeatJitterMs)*time.Millisecond

		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}

		e.resolveOne(ctx, decoyQuery{name: heartbeatHosts[e.rnd.Intn(len(heartbeatHosts))], qtype: dns.TypeA, source: provChatter})
	}
}

// pickPersona (#6) returns a sampled real-client persona to attribute a decoy to,
// and ok=false when attribution is off / no real clients exist yet. Lazily fills a
// small bounded pool from the source (one sample per call until full, then stops),
// then draws from it, so a given real client recurs with a stable IP/cookie/fp.
//
// ponytail: up to personaPoolSize DB samples over the engine's life, then none —
// no eviction (a household has few clients). Reset the pool if long-lived churn
// ever matters.
func (e *Engine) pickPersona() (querylog.ClientPersona, bool) {
	if !e.cfg.PersonaAttribution || e.source == nil {
		return querylog.ClientPersona{}, false
	}

	e.personaMu.Lock()
	defer e.personaMu.Unlock()

	if len(e.personas) < personaPoolSize {
		if p, err := e.source.SampleClient(); err == nil && p.IP != "" && !hasPersona(e.personas, p.IP) {
			e.personas = append(e.personas, p)
		}
	}

	if len(e.personas) == 0 {
		return querylog.ClientPersona{}, false
	}

	return e.personas[e.rnd.Intn(len(e.personas))], true
}

// vendorFamilyNames is the sorted list of vendorTelemetry family keys. Sorted (not
// map-iteration order) so phantom-family selection is deterministic under a seeded
// rng — the tests rely on it.
//
//nolint:gochecknoglobals
var vendorFamilyNames = sortedKeys(vendorTelemetry)

func sortedKeys(m map[string][]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}

	sort.Strings(ks)

	return ks
}

// classPersona attributes an emission to a real client and returns its effective
// device class. ok=false when persona attribution is off / cold start / class
// lookup fails, so emit falls back to the browsing path.
func (e *Engine) classPersona() (querylog.ClientPersona, string, bool) {
	persona, ok := e.pickPersona()
	if !ok {
		return persona, "", false
	}

	class, err := e.source.ClientClass(persona.IP)
	if err != nil {
		e.logger.WithError(err).Debug("client class lookup failed")

		return persona, "", false
	}

	return persona, class, true
}

// emitBeacon fires one IoT-shaped BEACON attributed to persona: a low-diversity
// lookup to a vendor-telemetry endpoint (or generic device chatter when vendor
// telemetry is disabled). One query, not a burst — IoT devices beacon, they don't
// browse; cadence/regularity is handled by the persona rate curve, not per-query.
func (e *Engine) emitBeacon(ctx context.Context, persona querylog.ClientPersona) {
	var q decoyQuery
	if e.cfg.DeviceClass.VendorTelemetry {
		q = e.beaconQuery()
	} else {
		q = e.chatterQuery()
	}

	q.persona = &persona
	e.resolveOne(ctx, q)
}

// beaconQuery returns one vendor-telemetry beacon. Qtype is A-dominant (low
// diversity, matching a real beacon). beaconFamily obscures the real fleet: a
// PhantomDevicesPct share draws from a family NOT configured as present, so the
// on-wire beacon mix can't be read for the site's true vendor or device count.
func (e *Engine) beaconQuery() decoyQuery {
	hosts := vendorTelemetry[e.beaconFamily()]

	qtype := dns.TypeA
	if e.rnd.Intn(5) == 0 {
		qtype = dns.TypeAAAA
	}

	return decoyQuery{name: hosts[e.rnd.Intn(len(hosts))], qtype: qtype, source: provBeacon}
}

// beaconFamily picks which vendor family to beacon to. With PhantomDevicesPct (or
// when no "real" families are configured) it draws a PHANTOM family — one not in
// cfg.VendorFamilies — so beacons to vendors the site doesn't run mask the real
// fleet's vendor and device count. Otherwise it beacons to a configured family.
func (e *Engine) beaconFamily() string {
	real := knownFamilies(e.cfg.DeviceClass.VendorFamilies)

	if len(real) == 0 || e.rndPct(e.cfg.DeviceClass.PhantomDevicesPct) {
		return e.phantomFamily(real)
	}

	return real[e.rnd.Intn(len(real))]
}

// phantomFamily returns a family not in exclude (a vendor the site may not run).
// Falls back to any family when every known family is excluded.
func (e *Engine) phantomFamily(exclude []string) string {
	pool := make([]string, 0, len(vendorFamilyNames))

	for _, f := range vendorFamilyNames {
		if !containsStr(exclude, f) {
			pool = append(pool, f)
		}
	}

	if len(pool) == 0 {
		pool = vendorFamilyNames
	}

	return pool[e.rnd.Intn(len(pool))]
}

// knownFamilies filters names to those with an embedded endpoint set (unknown
// family names in config are ignored rather than crashing).
func knownFamilies(names []string) []string {
	out := make([]string, 0, len(names))

	for _, n := range names {
		if _, ok := vendorTelemetry[n]; ok {
			out = append(out, n)
		}
	}

	return out
}

func containsStr(ss []string, s string) bool {
	return slices.Contains(ss, s)
}

// emitServer fires one SERVER-shaped lookup attributed to persona: a registry/
// update/monitoring endpoint (A record), not a human page load.
func (e *Engine) emitServer(ctx context.Context, persona querylog.ClientPersona) {
	q := decoyQuery{name: serverTelemetry[e.rnd.Intn(len(serverTelemetry))], qtype: dns.TypeA, source: provServer}
	q.persona = &persona
	e.resolveOne(ctx, q)
}

func hasPersona(ps []querylog.ClientPersona, ip string) bool {
	for _, p := range ps {
		if p.IP == ip {
			return true
		}
	}

	return false
}

// noteOutcome (#7) records a decoy resolve outcome in the rolling backoff window
// and adjusts the rate multiplier: when the recent error rate exceeds the
// threshold the multiplier is cut multiplicatively (floored at backoffMin);
// otherwise it recovers slowly toward 1. No-op when adaptive backoff is off.
//
// ponytail: best-effort throttle only. Decoys share the resolver chain, so we
// can't steer a specific decoy away from a failing upstream (same limitation as
// clientIP); reducing the aggregate rate is what's reachable, and it keeps the
// noise engine from piling onto a strained/rate-limiting upstream.
func (e *Engine) noteOutcome(failed bool) {
	if !e.cfg.AdaptiveBackoff {
		return
	}

	e.backoffMu.Lock()
	defer e.backoffMu.Unlock()

	e.outcomes = append(e.outcomes, failed)
	if len(e.outcomes) > backoffWindow {
		e.outcomes = e.outcomes[len(e.outcomes)-backoffWindow:]
	}

	if len(e.outcomes) < backoffMinSamples {
		return
	}

	errs := 0
	for _, f := range e.outcomes {
		if f {
			errs++
		}
	}

	prev := e.backoff

	if float64(errs)/float64(len(e.outcomes)) > backoffErrThreshold {
		e.backoff *= backoffDecay
		if e.backoff < backoffMin {
			e.backoff = backoffMin
		}
	} else {
		e.backoff += backoffRecover
		if e.backoff > 1 {
			e.backoff = 1
		}
	}

	// Log only the engage/recover transitions (crossing full rate), never per
	// outcome — the window is the incident signal, not each failed decoy.
	switch {
	case prev >= 1 && e.backoff < 1:
		e.logger.WithField("backoff", e.backoff).
			WithField("err_rate", float64(errs)/float64(len(e.outcomes))).
			Warn("decoy adaptive backoff engaged (upstream resolve errors); throttling decoy rate")
	case prev < 1 && e.backoff >= 1:
		e.logger.Info("decoy adaptive backoff recovered; decoy rate restored")
	}
}

// backoffFactor is the current decoy-rate multiplier (1 when adaptive backoff is
// off), applied to the effective QPM in nextInterval.
func (e *Engine) backoffFactor() float64 {
	if !e.cfg.AdaptiveBackoff {
		return 1
	}

	e.backoffMu.Lock()
	defer e.backoffMu.Unlock()

	return e.backoff
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

	e.stampFingerprint(req, fp)
}

// stampFingerprint rebuilds fp's wire shape onto req: qclass, 0x20 case, and a
// full OPT record (buffer size, DO, EDNS option codes in the sampled order). Used
// by both the name-keyed path (applyFingerprint) and the per-client persona path.
func (e *Engine) stampFingerprint(req *model.Request, fp querylog.FpSample) {
	if len(req.Req.Question) == 0 {
		return
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
		{name: "www." + reg, qtype: dns.TypeA, source: provCompanion},
		{name: clusterCompanions[e.rnd.Intn(len(clusterCompanions))], qtype: e.realQtype(), source: provCompanion},
	}

	fill := e.pickCompanions(2)
	if d, err := e.source.SampleList(); err == nil && d != "" {
		fill = append(fill, decoyQuery{name: d, qtype: e.realQtype(), source: provCompanion})
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
		{name: "www." + q.name, qtype: dns.TypeA, source: q.source}, // same site as the anchor
		{name: q.name, qtype: dns.TypeAAAA, source: q.source},
	}, e.pickCompanions(2)...)

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

	k := min(e.rnd.Intn(max+1), len(idx))

	out := make([]decoyQuery, 0, k)
	for _, i := range idx[:k] {
		out = append(out, decoyQuery{name: clusterCompanions[i], qtype: e.realQtype(), source: provCompanion})
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
	n := 2 + e.rnd.Intn(2)

	var b strings.Builder
	for range n {
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

// realQtype (T12/#4) picks a qtype from a realistic browser/OS mix: A and AAAA
// dominate, HTTPS(65) common, SVCB(64) rare, plus an occasional non-address type
// (TXT/MX/NS/SRV) that real clients emit (mail, discovery, DNSSEC tooling). A fixed
// distribution — cheaper than a per-query DB sample and close enough to the real
// shape; the A/AAAA pairing is added separately in maybeDualStack. PTR is emitted
// only by the chatter path, where it carries an in-addr.arpa name.
func (e *Engine) realQtype() uint16 {
	switch r := e.rnd.Intn(100); { //nolint:mnd // fixed realistic qtype mix
	case r < 42:
		return dns.TypeA
	case r < 78:
		return dns.TypeAAAA
	case r < 90:
		return dns.TypeHTTPS
	case r < 93:
		return dns.TypeSVCB
	case r < 96:
		return dns.TypeTXT
	case r < 98:
		return dns.TypeMX
	case r < 99:
		return dns.TypeNS
	default:
		return dns.TypeSRV
	}
}

func qtypeFromString(s string) uint16 {
	if t, ok := dns.StringToType[s]; ok {
		return t
	}

	return dns.TypeA
}
