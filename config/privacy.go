package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// PrivacyConfig groups the fork's privacy/decoy features: background decoy
// (noise) query generation, TTL jitter on cached answers, and EDNS(0) padding
// of encrypted-upstream queries.
type PrivacyConfig struct {
	Decoy       DecoyConfig       `yaml:"decoy"`
	TTLJitter   TTLJitterConfig   `yaml:"ttlJitter"`
	EDNSPadding EDNSPaddingConfig `yaml:"ednsPadding"`
	// QueryCaseRandomization applies DNS 0x20 case randomization (draft-vixie-dnsext-dns0x20)
	// to outgoing forwarded queries: each ASCII letter of the question name is independently
	// upper/lower-cased on the wire, the upstream must echo the exact case (spoofing defense),
	// and the answer is normalized back to the client's case. Forwarding path only; the
	// recursive (zdns) path ignores it until the documented zdns fork lands.
	QueryCaseRandomization bool `default:"false" yaml:"queryCaseRandomization"`
	// Profiling is the opt-in, default-OFF presence/sleep/work analysis. Local-only,
	// never exported. See ProfilingConfig.
	Profiling ProfilingConfig `yaml:"profiling"`
	// ShadowBlockedQueries decouples the client answer from the wire query for blocked
	// domains: the client still gets the block response (ad-blocking intact), but the
	// real upstream query is also egressed asynchronously and its answer discarded, so
	// real page-load cohorts stay complete on the wire and dissolve the "blocking
	// resolver" signature. Default ON, but it only ACTIVATES when the decoy engine is
	// also enabled (privacy.decoy.enable) — that is what gives the egressed tracker
	// queries their decoy cover. Without the noise engine it stays inert, so a
	// blocking-only setup never egresses blocked-domain queries uncovered.
	ShadowBlockedQueries bool `default:"true" yaml:"shadowBlockedQueries"`
}

// DecoyConfig configures the background noise/decoy query engine. Decoy
// queries are marked Bypass so they never touch the cache (neither served from
// it nor stored in it).
type DecoyConfig struct {
	Enable           bool    `default:"false" yaml:"enable"`
	QueriesPerMinute float64 `default:"4"     yaml:"queriesPerMinute"`
	// Source-weighted noise repartition (pick source by weight, then a domain
	// within it — the list's ~1.8M count never affects source odds). Large spread
	// so the user's own domains dominate: replay+corpus (visited) : list ≈ 20:1.
	// Companions are a separate, higher tier — triggered per real query
	// (companionPct), so realistic page-load bursts dominate the noise overall.
	ReplayWeight     uint   `default:"15"      yaml:"replayWeight"`     // recent-real replay pool (7-day window)
	CorpusWeight     uint   `default:"5"       yaml:"corpusWeight"`     // persistent all-time visited-domains corpus
	ListWeight       uint   `default:"1"       yaml:"listWeight"`       // Tranco 1M public static list (thin breadth layer)
	ActiveHoursStart int    `default:"0"       yaml:"activeHoursStart"` // 0-23; full rate within [start,end), floor rate outside
	ActiveHoursEnd   int    `default:"24"      yaml:"activeHoursEnd"`   // 1-24
	RefreshURL       string `yaml:"refreshURL"`                         // optional live list source; empty = embedded

	// Obfuscation techniques (all default-on; each independently toggleable).
	DiurnalShaping   bool `default:"true" yaml:"diurnalShaping"`   // scale rate by real per-hour volume
	ReplayMutate     bool `default:"true" yaml:"replayMutate"`     // mutate replayed queries so they aren't byte-identical
	FingerprintMatch bool `default:"true" yaml:"fingerprintMatch"` // stamp EDNS shape sampled from real clients
	SplitUpstream    bool `default:"true" yaml:"splitUpstream"`    // vary decoy client identity to diverge group routing
	MissChaffPct     uint `default:"15"   yaml:"missChaffPct"`     // % of decoys that are likely-NXDOMAIN random labels
	ClusterPct       uint `default:"20"   yaml:"clusterPct"`       // % of timer emissions that fire a small related burst (secondary to CompanionPct)

	// Reactive obfuscation: track live real traffic instead of the 7-day historical shape.
	ReactiveVolume bool `default:"true" yaml:"reactiveVolume"` // rate tracks live recent real QPS (± jitter); historical diurnal is the cold-start fallback (only used when personaCover is off)
	CompanionPct   uint `default:"40"   yaml:"companionPct"`   // % of real queries that trigger a browse-style companion cluster derived from that domain

	// Structural emission (7G/#1/#5): shape the SEQUENCE and TEXTURE of decoy
	// emissions after real browsing instead of IID picks.
	CohortPct        uint `default:"55"   yaml:"cohortPct"`        // % of structural emissions that replay a whole RECORDED page-load cohort (real timing/texture, incl. blocked members) instead of a synthetic single/cluster; falls back to the synthetic path at cold start
	SessionCoherence bool `default:"true" yaml:"sessionCoherence"` // walk plausible session chains (NextInSession/SessionSeed) instead of IID domain picks
	StepPct          uint `default:"70"   yaml:"stepPct"`          // within a session, % chance to advance to a topically-plausible successor vs. reseeding a fresh session
	RevisitCadence   bool `default:"true" yaml:"revisitCadence"`   // re-emit corpus/replay domains on their learned revisit interval (jittered) instead of flat random
	// Cohort-replay perturbation: keep the recorded texture but break the exact
	// 1:1 echo — jitter each sub-resource offset (the re-sort turns overlapping
	// jitter into small run-to-run reordering) and splice in a small % companion.
	CohortJitterMs     uint `default:"120" yaml:"cohortJitterMs"`     // ± ms jitter on each recorded sub-resource's replay offset (0 = exact 1:1 replay)
	CohortCompanionPct uint `default:"15"  yaml:"cohortCompanionPct"` // % of cohort replays that splice in one unrelated companion at a random point in the load

	// Compensating persona cover (#8): TOTAL egress tracks a household diurnal
	// target curve, so decoy_rate(t) = max(0, targetCurve(t) − recentRealQPM).
	// This hides the activity LEVEL (not just which queries are real) without
	// delaying real queries — the box always looks like a generic household on
	// the wire. When off, the engine falls back to the reactive/diurnal path.
	//
	// Residual: real usage ABOVE the peak ceiling still spikes total egress — the
	// curve hides level only up to targetQpmPeak, by design (a truly constant
	// max-rate cover would cost fixed bandwidth we don't pay on a home box).
	PersonaCover bool `default:"true" yaml:"personaCover"`
	// PersonaProfile selects the persona-curve preset the compensating cover aims
	// for: "home" (peak/trough 40/6), "enterprise" (300/60, office-diurnal over a
	// 24/7 IoT floor), or "auto" (home baseline; the engine may escalate toward the
	// enterprise curve from observed client classes). The explicit TargetQPM* fields
	// still override the preset (see EffectivePersonaCurve). "auto" is the default so
	// a home box behaves exactly as before this field existed.
	PersonaProfile  string  `default:"auto" yaml:"personaProfile"`
	TargetQPMPeak   float64 `default:"40"   yaml:"targetQpmPeak"`   // busy-hour target total egress (queries/min); the shipped home default
	TargetQPMTrough float64 `default:"6"    yaml:"targetQpmTrough"` // pre-dawn quiet target total egress (queries/min); the shipped home default

	// Device-class awareness: classify each client from its real DNS behavior
	// (iot / workstation / server / unknown) and shape its cover accordingly.
	DeviceClass DeviceClassConfig `yaml:"deviceClass"`

	// Per-query realism + operational (device chatter, transport/qtype/failure
	// diversity, per-client personas, adaptive back-off).
	ChatterPct         uint `default:"15"   yaml:"chatterPct"`         // % of emissions that are device background chatter (connectivity/NTP/telemetry + RFC1918 PTR) instead of browsing
	TCPPct             uint `default:"10"   yaml:"tcpPct"`             // % of decoys stamped as TCP transport (req.Protocol); see engine limitation — blocky→upstream transport is upstream-config-global, not per-query
	FailChaffPct       uint `default:"8"    yaml:"failChaffPct"`       // % of emissions that deliberately query a likely-NXDOMAIN name so decoys don't always succeed
	PersonaAttribution bool `default:"true" yaml:"personaAttribution"` // attribute decoys to sampled real clients (stamp their ClientIP + fingerprint) so each client's on-wire profile stays consistent under noise
	AdaptiveBackoff    bool `default:"true" yaml:"adaptiveBackoff"`    // reduce the decoy rate when the recent decoy resolve error rate spikes (upstream strain / rate-limiting) and recover slowly

	// Wire-egress hardening.
	ShadowTTL                bool    `default:"true" yaml:"shadowTTL"`                // suppress re-emitting a decoy (name,qtype) within its own observed answer TTL
	DualStackPct             uint    `default:"55"   yaml:"dualStackPct"`             // % of A/AAAA decoys that also emit the sibling record (browser dual-stack)
	OffHoursFloorQPM         float64 `default:"0.5"  yaml:"offHoursFloorQPM"`         // always-on Poisson floor rate outside active hours (never gate fully to zero)
	ActiveHoursEdgeJitterMin int     `default:"30"   yaml:"activeHoursEdgeJitterMin"` // ± minutes of per-day jitter on the active-hours window edges

	// Corpus pre-warming: proactively pull TRENDING/RISING domains into the noise
	// corpus before the user first visits them, so a genuinely new domain is
	// already covered by chaff. Offline-first: with no PrewarmURL it mines the
	// embedded Tranco list's mid-popularity band (ranks ~1k-50k, the domains a
	// normal person is likely to newly encounter), rotating the slab each run.
	PrewarmEnable        bool   `default:"true"    yaml:"prewarmEnable"`
	PrewarmURL           string `yaml:"prewarmURL"` // optional trending source (Tranco rising / CF Radar CSV/txt); empty = embedded band
	PrewarmIntervalHours uint   `default:"12"      yaml:"prewarmIntervalHours"`
}

// DeviceClassConfig controls per-device-class decoy shaping. IoT devices beacon
// to fixed vendor telemetry (they do not browse) and servers hit
// registry/update/monitoring endpoints; emitting BROWSING-shaped cover for them
// is itself a tell. When Enable is on, the engine reads each client's cached
// class (querylog.DecoySource.ClientClass) and shapes its chaff to match.
type DeviceClassConfig struct {
	// Enable turns on classification + per-class shaping. Off = every client gets
	// the browsing-shaped cover (pre-device-class behaviour).
	Enable bool `default:"true" yaml:"enable"`
	// VendorTelemetry emits vendor-telemetry chaff (fixed beacon endpoints) for
	// iot/server-classed clients instead of browse companions.
	VendorTelemetry bool `default:"true" yaml:"vendorTelemetry"`
	// VendorFamilies names which telemetry families to draw beacon domains from
	// (e.g. "apple", "google", "amazon", "microsoft", "samsung", "tuya", "sonos").
	// Empty = the engine's built-in default family set.
	VendorFamilies []string `yaml:"vendorFamilies"`
	// PhantomDevicesPct is the share of vendor-telemetry chaff drawn from families
	// NOT present in the real fleet, to obscure true fleet size and vendor mix.
	// 0 = only mirror observed vendors.
	PhantomDevicesPct uint `default:"20" yaml:"phantomDevicesPct"`
}

// knownVendorFamilies mirrors the decoy engine's embedded vendorTelemetry map
// keys (decoy/engine.go). Keep in sync: a family added to the engine must be
// added here too, or configs naming it are rejected at validation.
//
//nolint:gochecknoglobals
var knownVendorFamilies = []string{
	"ecobee", "enphase", "fronius", "goodwe", "growatt", "hue", "huawei", "nest",
	"ring", "shelly", "sma", "solaredge", "sonos", "tesla", "tuya", "victron",
}

// personaPreset is a (peak, trough) queries/min target curve.
type personaPreset struct{ Peak, Trough float64 }

const (
	homeDefaultPeak   = 40 // must match TargetQPMPeak's default tag
	homeDefaultTrough = 6  // must match TargetQPMTrough's default tag
)

// personaPresets maps PersonaProfile to its busy-hour/quiet-hour target curve.
var personaPresets = map[string]personaPreset{
	"home":       {Peak: homeDefaultPeak, Trough: homeDefaultTrough},
	"auto":       {Peak: homeDefaultPeak, Trough: homeDefaultTrough},
	"enterprise": {Peak: 300, Trough: 60},
}

// EffectivePersonaCurve resolves the compensating-cover target curve the engine
// should aim for: the PersonaProfile preset, with any explicit TargetQPM* value
// overriding it. This is what the engine consumes instead of the raw fields.
//
// ponytail: an override is detected as "field differs from the shipped home
// default". So an enterprise deployment that genuinely wants exactly 40/6 must
// nudge it (e.g. 40.0001); harmless. Add an explicit *float64 sentinel only if a
// user ever hits that.
func (c *DecoyConfig) EffectivePersonaCurve() (peak, trough float64) {
	peak, trough = c.TargetQPMPeak, c.TargetQPMTrough

	if p, ok := personaPresets[c.PersonaProfile]; ok {
		if peak == homeDefaultPeak {
			peak = p.Peak
		}

		if trough == homeDefaultTrough {
			trough = p.Trough
		}
	}

	return peak, trough
}

// ProfilingConfig controls the opt-in presence/sleep/work profiling. It is a
// privacy feature (surfaces WHEN each device is active), so it is OFF by default
// and its data is local-only and never exported. TZ is a required calibration
// knob: the hourly aggregates are UTC, so a wrong zone makes sleep/work windows
// silently wrong by hours.
type ProfilingConfig struct {
	Enable bool   `default:"false" yaml:"enable"`
	TZ     string `yaml:"tz"` // IANA name (e.g. "Europe/Zurich"); empty = UTC
}

func (c *ProfilingConfig) validate() error {
	if c.Enable && c.TZ != "" {
		if _, err := time.LoadLocation(c.TZ); err != nil {
			return fmt.Errorf("privacy.profiling: tz (%q) is not a valid IANA time zone: %w", c.TZ, err)
		}
	}

	return nil
}

// TTLJitterConfig randomizes cached-answer TTLs by +/- PercentPct percent.
type TTLJitterConfig struct {
	Enable     bool `default:"false" yaml:"enable"`
	PercentPct uint `default:"10"    yaml:"percent"` // +/- percent
}

// EDNSPaddingConfig enables EDNS(0) padding (RFC 7830) on encrypted-upstream queries.
type EDNSPaddingConfig struct {
	Enable bool `default:"false" yaml:"enable"`
}

func (c *PrivacyConfig) validate(_ *logrus.Entry) error {
	if err := c.Decoy.validate(); err != nil {
		return err
	}

	if err := c.Profiling.validate(); err != nil {
		return err
	}

	return c.TTLJitter.validate()
}

func (c *DecoyConfig) validate() error {
	if !c.Enable {
		return nil
	}

	if c.ActiveHoursStart < 0 || c.ActiveHoursStart > 23 {
		return fmt.Errorf("privacy.decoy: activeHoursStart (%d) must be in [0, 23]", c.ActiveHoursStart)
	}

	if c.ActiveHoursEnd < 1 || c.ActiveHoursEnd > 24 {
		return fmt.Errorf("privacy.decoy: activeHoursEnd (%d) must be in [1, 24]", c.ActiveHoursEnd)
	}

	if c.ActiveHoursStart >= c.ActiveHoursEnd {
		return fmt.Errorf("privacy.decoy: activeHoursStart (%d) must be < activeHoursEnd (%d)",
			c.ActiveHoursStart, c.ActiveHoursEnd)
	}

	if c.ReplayWeight == 0 && c.CorpusWeight == 0 && c.ListWeight == 0 {
		return errors.New("privacy.decoy: replayWeight, corpusWeight and listWeight must not all be zero when enabled")
	}

	// Bound each weight so the sum can never reach 2^31: the engine computes
	// int(replay+corpus+list) for rand.Intn, and an overflowed (negative) sum
	// passes the zero guard and panics on 32-bit ARM.
	const maxDecoyWeight = 1_000_000

	for _, w := range []struct {
		name string
		val  uint
	}{
		{"replayWeight", c.ReplayWeight},
		{"corpusWeight", c.CorpusWeight},
		{"listWeight", c.ListWeight},
	} {
		if w.val > maxDecoyWeight {
			return fmt.Errorf("privacy.decoy: %s (%d) must be in [0, %d]", w.name, w.val, maxDecoyWeight)
		}
	}

	if c.DualStackPct > 100 {
		return fmt.Errorf("privacy.decoy: dualStackPct (%d) must be in [0, 100]", c.DualStackPct)
	}

	if c.OffHoursFloorQPM < 0 {
		return fmt.Errorf("privacy.decoy: offHoursFloorQPM (%.2f) must be >= 0", c.OffHoursFloorQPM)
	}

	if c.ActiveHoursEdgeJitterMin < 0 || c.ActiveHoursEdgeJitterMin > 720 {
		return fmt.Errorf("privacy.decoy: activeHoursEdgeJitterMin (%d) must be in [0, 720]", c.ActiveHoursEdgeJitterMin)
	}

	if c.MissChaffPct > 100 {
		return fmt.Errorf("privacy.decoy: missChaffPct (%d) must be in [0, 100]", c.MissChaffPct)
	}

	if c.ClusterPct > 100 {
		return fmt.Errorf("privacy.decoy: clusterPct (%d) must be in [0, 100]", c.ClusterPct)
	}

	if c.CompanionPct > 100 {
		return fmt.Errorf("privacy.decoy: companionPct (%d) must be in [0, 100]", c.CompanionPct)
	}

	if c.CohortPct > 100 {
		return fmt.Errorf("privacy.decoy: cohortPct (%d) must be in [0, 100]", c.CohortPct)
	}

	if c.StepPct > 100 {
		return fmt.Errorf("privacy.decoy: stepPct (%d) must be in [0, 100]", c.StepPct)
	}

	if c.CohortCompanionPct > 100 {
		return fmt.Errorf("privacy.decoy: cohortCompanionPct (%d) must be in [0, 100]", c.CohortCompanionPct)
	}

	if c.ChatterPct > 100 {
		return fmt.Errorf("privacy.decoy: chatterPct (%d) must be in [0, 100]", c.ChatterPct)
	}

	if c.TCPPct > 100 {
		return fmt.Errorf("privacy.decoy: tcpPct (%d) must be in [0, 100]", c.TCPPct)
	}

	if c.FailChaffPct > 100 {
		return fmt.Errorf("privacy.decoy: failChaffPct (%d) must be in [0, 100]", c.FailChaffPct)
	}

	if _, ok := personaPresets[c.PersonaProfile]; !ok {
		return fmt.Errorf("privacy.decoy: personaProfile (%q) must be one of home, enterprise, auto", c.PersonaProfile)
	}

	peak, trough := c.EffectivePersonaCurve()
	if trough < 0 {
		return fmt.Errorf("privacy.decoy: targetQpmTrough (%.2f) must be >= 0", trough)
	}

	if peak < trough {
		return fmt.Errorf("privacy.decoy: targetQpmPeak (%.2f) must be >= targetQpmTrough (%.2f)", peak, trough)
	}

	if c.DeviceClass.PhantomDevicesPct > 100 {
		return fmt.Errorf("privacy.decoy: deviceClass.phantomDevicesPct (%d) must be in [0, 100]",
			c.DeviceClass.PhantomDevicesPct)
	}

	// The decoy engine exact-matches lowercase vendorTelemetry keys and silently
	// drops everything else — an all-unknown list would flip 100% of beacons to
	// phantom mode. Reject unknowns here and normalize case in place so the
	// engine's exact match behaves case-insensitively.
	if c.DeviceClass.Enable && c.DeviceClass.VendorTelemetry {
		for i, f := range c.DeviceClass.VendorFamilies {
			name := strings.ToLower(strings.TrimSpace(f))
			if !slices.Contains(knownVendorFamilies, name) {
				return fmt.Errorf("privacy.decoy: deviceClass.vendorFamilies entry %q is unknown; known families: %s",
					f, strings.Join(knownVendorFamilies, ", "))
			}

			c.DeviceClass.VendorFamilies[i] = name
		}
	}

	// The prewarm interval becomes a time.Duration in hours. Zero makes
	// time.NewTicker panic outright, and a large enough value overflows int64
	// into a negative duration, which panics the same way — either would take
	// the process down at startup rather than surfacing as a config error.
	if c.PrewarmEnable && (c.PrewarmIntervalHours == 0 || c.PrewarmIntervalHours > MaxIntervalHours) {
		return fmt.Errorf("privacy.decoy: prewarmIntervalHours must be in [1, %d] when prewarm is enabled",
			MaxIntervalHours)
	}

	return nil
}

func (c *TTLJitterConfig) validate() error {
	if !c.Enable {
		return nil
	}

	const maxJitterPct = 90
	if c.PercentPct > maxJitterPct {
		return fmt.Errorf("privacy.ttlJitter: percent (%d) must be in [0, %d]", c.PercentPct, maxJitterPct)
	}

	return nil
}
