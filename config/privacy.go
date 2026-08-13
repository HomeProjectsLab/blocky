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
}

// DecoyConfig configures the background noise/decoy query engine. Decoy
// queries are marked Bypass so they never touch the cache (neither served from
// it nor stored in it).
type DecoyConfig struct {
	Enable           bool    `yaml:"enable" default:"false"`
	QueriesPerMinute float64 `yaml:"queriesPerMinute" default:"4"`
	ReplayWeight     uint    `yaml:"replayWeight" default:"10"`    // real-query replay pool weight
	ListWeight       uint    `yaml:"listWeight" default:"1"`       // Tranco 1M static list weight
	ActiveHoursStart int     `yaml:"activeHoursStart" default:"0"` // 0-23; noise only within [start,end)
	ActiveHoursEnd   int     `yaml:"activeHoursEnd" default:"24"`  // 1-24
	RefreshURL       string  `yaml:"refreshURL"`                   // optional live list source; empty = embedded

	// Obfuscation techniques (all default-on; each independently toggleable).
	DiurnalShaping   bool `yaml:"diurnalShaping" default:"true"`   // scale rate by real per-hour volume
	ReplayMutate     bool `yaml:"replayMutate" default:"true"`     // mutate replayed queries so they aren't byte-identical
	FingerprintMatch bool `yaml:"fingerprintMatch" default:"true"` // stamp EDNS shape sampled from real clients
	SplitUpstream    bool `yaml:"splitUpstream" default:"true"`    // vary decoy client identity to diverge group routing
	MissChaffPct     uint `yaml:"missChaffPct" default:"15"`       // % of decoys that are likely-NXDOMAIN random labels
	ClusterPct       uint `yaml:"clusterPct" default:"20"`         // % of emissions that fire a small related burst
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

	if c.ReplayWeight == 0 && c.ListWeight == 0 {
		return fmt.Errorf("privacy.decoy: replayWeight and listWeight must not both be zero when enabled")
	}

	if c.MissChaffPct > 100 {
		return fmt.Errorf("privacy.decoy: missChaffPct (%d) must be in [0, 100]", c.MissChaffPct)
	}

	if c.ClusterPct > 100 {
		return fmt.Errorf("privacy.decoy: clusterPct (%d) must be in [0, 100]", c.ClusterPct)
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
