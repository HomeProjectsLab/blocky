package config

import (
	"fmt"

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
	QueryCaseRandomization bool `yaml:"queryCaseRandomization" default:"false"`
	// ShadowBlockedQueries decouples the client answer from the wire query for blocked
	// domains: the client still gets the block response (ad-blocking intact), but the
	// real upstream query is also egressed asynchronously and its answer discarded, so
	// real page-load cohorts stay complete on the wire and dissolve the "blocking
	// resolver" signature. Default ON, but it only ACTIVATES when the decoy engine is
	// also enabled (privacy.decoy.enable) — that is what gives the egressed tracker
	// queries their decoy cover. Without the noise engine it stays inert, so a
	// blocking-only setup never egresses blocked-domain queries uncovered.
	ShadowBlockedQueries bool `yaml:"shadowBlockedQueries" default:"true"`
}

// DecoyConfig configures the background noise/decoy query engine. Decoy
// queries are marked Bypass so they never touch the cache (neither served from
// it nor stored in it).
type DecoyConfig struct {
	Enable           bool    `yaml:"enable" default:"false"`
	QueriesPerMinute float64 `yaml:"queriesPerMinute" default:"4"`
	// Source-weighted noise repartition (pick source by weight, then a domain
	// within it — the list's ~1.8M count never affects source odds). Large spread
	// so the user's own domains dominate: replay+corpus (visited) : list ≈ 20:1.
	// Companions are a separate, higher tier — triggered per real query
	// (companionPct), so realistic page-load bursts dominate the noise overall.
	ReplayWeight     uint   `yaml:"replayWeight" default:"15"`    // recent-real replay pool (7-day window)
	CorpusWeight     uint   `yaml:"corpusWeight" default:"5"`     // persistent all-time visited-domains corpus
	ListWeight       uint   `yaml:"listWeight" default:"1"`       // Tranco 1M public static list (thin breadth layer)
	ActiveHoursStart int    `yaml:"activeHoursStart" default:"0"` // 0-23; full rate within [start,end), floor rate outside
	ActiveHoursEnd   int    `yaml:"activeHoursEnd" default:"24"`  // 1-24
	RefreshURL       string `yaml:"refreshURL"`                   // optional live list source; empty = embedded

	// Obfuscation techniques (all default-on; each independently toggleable).
	DiurnalShaping   bool `yaml:"diurnalShaping" default:"true"`   // scale rate by real per-hour volume
	ReplayMutate     bool `yaml:"replayMutate" default:"true"`     // mutate replayed queries so they aren't byte-identical
	FingerprintMatch bool `yaml:"fingerprintMatch" default:"true"` // stamp EDNS shape sampled from real clients
	SplitUpstream    bool `yaml:"splitUpstream" default:"true"`    // vary decoy client identity to diverge group routing
	MissChaffPct     uint `yaml:"missChaffPct" default:"15"`       // % of decoys that are likely-NXDOMAIN random labels
	ClusterPct       uint `yaml:"clusterPct" default:"20"`         // % of timer emissions that fire a small related burst (secondary to CompanionPct)

	// Reactive obfuscation: track live real traffic instead of the 7-day historical shape.
	ReactiveVolume bool `yaml:"reactiveVolume" default:"true"` // rate tracks live recent real QPS (± jitter); historical diurnal is the cold-start fallback (only used when personaCover is off)
	CompanionPct   uint `yaml:"companionPct" default:"40"`     // % of real queries that trigger a browse-style companion cluster derived from that domain

	// Structural emission (7G/#1/#5): shape the SEQUENCE and TEXTURE of decoy
	// emissions after real browsing instead of IID picks.
	CohortPct        uint `yaml:"cohortPct" default:"55"`          // % of structural emissions that replay a whole RECORDED page-load cohort (real timing/texture, incl. blocked members) instead of a synthetic single/cluster; falls back to the synthetic path at cold start
	SessionCoherence bool `yaml:"sessionCoherence" default:"true"` // walk plausible session chains (NextInSession/SessionSeed) instead of IID domain picks
	StepPct          uint `yaml:"stepPct" default:"70"`            // within a session, % chance to advance to a topically-plausible successor vs. reseeding a fresh session
	RevisitCadence   bool `yaml:"revisitCadence" default:"true"`   // re-emit corpus/replay domains on their learned revisit interval (jittered) instead of flat random

	// Compensating persona cover (#8): TOTAL egress tracks a household diurnal
	// target curve, so decoy_rate(t) = max(0, targetCurve(t) − recentRealQPM).
	// This hides the activity LEVEL (not just which queries are real) without
	// delaying real queries — the box always looks like a generic household on
	// the wire. When off, the engine falls back to the reactive/diurnal path.
	//
	// Residual: real usage ABOVE the peak ceiling still spikes total egress — the
	// curve hides level only up to targetQpmPeak, by design (a truly constant
	// max-rate cover would cost fixed bandwidth we don't pay on a home box).
	PersonaCover    bool    `yaml:"personaCover" default:"true"`
	TargetQPMPeak   float64 `yaml:"targetQpmPeak" default:"40"`  // busy-hour target total egress (queries/min)
	TargetQPMTrough float64 `yaml:"targetQpmTrough" default:"6"` // pre-dawn quiet target total egress (queries/min)

	// Per-query realism + operational (device chatter, transport/qtype/failure
	// diversity, per-client personas, adaptive back-off).
	ChatterPct         uint `yaml:"chatterPct" default:"15"`           // % of emissions that are device background chatter (connectivity/NTP/telemetry + RFC1918 PTR) instead of browsing
	TCPPct             uint `yaml:"tcpPct" default:"10"`               // % of decoys stamped as TCP transport (req.Protocol); see engine limitation — blocky→upstream transport is upstream-config-global, not per-query
	FailChaffPct       uint `yaml:"failChaffPct" default:"8"`          // % of emissions that deliberately query a likely-NXDOMAIN name so decoys don't always succeed
	PersonaAttribution bool `yaml:"personaAttribution" default:"true"` // attribute decoys to sampled real clients (stamp their ClientIP + fingerprint) so each client's on-wire profile stays consistent under noise
	AdaptiveBackoff    bool `yaml:"adaptiveBackoff" default:"true"`    // reduce the decoy rate when the recent decoy resolve error rate spikes (upstream strain / rate-limiting) and recover slowly

	// Wire-egress hardening.
	ShadowTTL                bool    `yaml:"shadowTTL" default:"true"`              // suppress re-emitting a decoy (name,qtype) within its own observed answer TTL
	DualStackPct             uint    `yaml:"dualStackPct" default:"55"`             // % of A/AAAA decoys that also emit the sibling record (browser dual-stack)
	OffHoursFloorQPM         float64 `yaml:"offHoursFloorQPM" default:"0.5"`        // always-on Poisson floor rate outside active hours (never gate fully to zero)
	ActiveHoursEdgeJitterMin int     `yaml:"activeHoursEdgeJitterMin" default:"30"` // ± minutes of per-day jitter on the active-hours window edges

	// Corpus pre-warming: proactively pull TRENDING/RISING domains into the noise
	// corpus before the user first visits them, so a genuinely new domain is
	// already covered by chaff. Offline-first: with no PrewarmURL it mines the
	// embedded Tranco list's mid-popularity band (ranks ~1k-50k, the domains a
	// normal person is likely to newly encounter), rotating the slab each run.
	PrewarmEnable        bool   `yaml:"prewarmEnable" default:"true"`
	PrewarmURL           string `yaml:"prewarmURL"` // optional trending source (Tranco rising / CF Radar CSV/txt); empty = embedded band
	PrewarmIntervalHours uint   `yaml:"prewarmIntervalHours" default:"12"`
}

// TTLJitterConfig randomizes cached-answer TTLs by +/- PercentPct percent.
type TTLJitterConfig struct {
	Enable     bool `yaml:"enable" default:"false"`
	PercentPct uint `yaml:"percent" default:"10"` // +/- percent
}

// EDNSPaddingConfig enables EDNS(0) padding (RFC 7830) on encrypted-upstream queries.
type EDNSPaddingConfig struct {
	Enable bool `yaml:"enable" default:"false"`
}

func (c *PrivacyConfig) validate(_ *logrus.Entry) error {
	if err := c.Decoy.validate(); err != nil {
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
		return fmt.Errorf("privacy.decoy: replayWeight, corpusWeight and listWeight must not all be zero when enabled")
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

	if c.ChatterPct > 100 {
		return fmt.Errorf("privacy.decoy: chatterPct (%d) must be in [0, 100]", c.ChatterPct)
	}

	if c.TCPPct > 100 {
		return fmt.Errorf("privacy.decoy: tcpPct (%d) must be in [0, 100]", c.TCPPct)
	}

	if c.FailChaffPct > 100 {
		return fmt.Errorf("privacy.decoy: failChaffPct (%d) must be in [0, 100]", c.FailChaffPct)
	}

	if c.TargetQPMTrough < 0 {
		return fmt.Errorf("privacy.decoy: targetQpmTrough (%.2f) must be >= 0", c.TargetQPMTrough)
	}

	if c.TargetQPMPeak < c.TargetQPMTrough {
		return fmt.Errorf("privacy.decoy: targetQpmPeak (%.2f) must be >= targetQpmTrough (%.2f)",
			c.TargetQPMPeak, c.TargetQPMTrough)
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
