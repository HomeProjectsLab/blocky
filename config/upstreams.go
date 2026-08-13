package config

import (
	"fmt"

	"github.com/0xERR0R/blocky/log"
	"github.com/sirupsen/logrus"
)

const UpstreamDefaultCfgName = "default"

// QUICConfig holds QUIC-specific upstream settings.
type QUICConfig struct {
	// Maximum idle duration before the QUIC connection is closed.
	MaxIdleTimeout Duration `default:"30s" yaml:"maxIdleTimeout"`
	// Interval at which keep-alive packets are sent to maintain the QUIC connection.
	KeepAlivePeriod Duration `default:"15s" yaml:"keepAlivePeriod"`
}

// Upstreams upstream servers configuration
type Upstreams struct {
	// Initialization strategy controlling when upstream resolvers are tested on startup.
	Init Init `yaml:"init"`
	// Timeout for upstream DNS connections; a value <= 0 is reset to the default.
	Timeout Duration `default:"2s" yaml:"timeout"`
	// Named groups of upstream DNS resolvers; the "default" group is required.
	Groups UpstreamGroups `yaml:"groups"`
	// Strategy for selecting which upstream(s) to use per query (parallel_best, random, strict).
	Strategy UpstreamStrategy `default:"parallel_best" yaml:"strategy"`
	// HTTP User-Agent header sent when connecting to DoH upstream servers.
	UserAgent string `yaml:"userAgent"`
	// QUIC-specific connection settings used when DoQ upstreams are configured.
	QUIC QUICConfig `yaml:"quic"`
	// Per-group settings overriding the global strategy.
	GroupConfig map[string]UpstreamGroupConfig `yaml:"groupConfig"`
	// DomainShard holds settings for the domain_shard strategy.
	DomainShard DomainShardConfig `yaml:"domainShard"`
	// EDNSPadding pads outgoing queries on encrypted transports (DoT/DoH/DoQ) to a block
	// boundary (RFC 7830). Runtime-derived from privacy.ednsPadding, not user-set here.
	EDNSPadding bool `yaml:"-"`
	// QueryCaseRandomization applies DNS 0x20 case randomization to outgoing forwarded
	// queries. Runtime-derived from privacy.queryCaseRandomization, not user-set here.
	QueryCaseRandomization bool `yaml:"-"`
}

// DomainShardConfig holds settings for the domain_shard strategy.
type DomainShardConfig struct {
	// Hours between shard-salt rotations. The domain->upstream mapping is stable
	// within a rotation window and moves across windows, so no single upstream
	// keeps a permanent stable slice of the user's domains. 0 disables rotation
	// (a permanent, fingerprintable mapping). Trade-off: rotation spreads a
	// domain's history across upstreams over time but loses upstream cache
	// locality at each window boundary.
	RotateHours uint `yaml:"rotateHours" default:"24"`
}

// UpstreamGroupConfig holds per-group settings overriding the global ones.
type UpstreamGroupConfig struct {
	// Strategy for this group; zero/absent falls back to the global Upstreams.Strategy.
	Strategy UpstreamStrategy `yaml:"strategy" default:"parallel_best"`
	// Minimum duration to stick to one upstream when strategy is time_hop.
	HopMin Duration `yaml:"hopMin" default:"5m"`
	// Maximum duration to stick to one upstream when strategy is time_hop.
	HopMax Duration `yaml:"hopMax" default:"30m"`
}

// EffectiveStrategy returns the strategy for the given group: the group's
// override if set, otherwise the global strategy.
//
// Note: the zero value of UpstreamStrategy is parallel_best, so a group can't
// override a non-parallel_best global strategy back to parallel_best.
func (c *Upstreams) EffectiveStrategy(group string) UpstreamStrategy {
	if gc, ok := c.GroupConfig[group]; ok && gc.Strategy != UpstreamStrategyParallelBest {
		return gc.Strategy
	}

	return c.Strategy
}

// GroupSettings returns the group's settings resolved with defaults:
// strategy falls back to the global one, hopMin/hopMax to their defaults.
func (c *Upstreams) GroupSettings(group string) UpstreamGroupConfig {
	gc := c.GroupConfig[group]
	gc.Strategy = c.EffectiveStrategy(group)

	defaults := mustDefault[UpstreamGroupConfig]()

	if !gc.HopMin.IsAboveZero() {
		gc.HopMin = defaults.HopMin
	}

	if !gc.HopMax.IsAboveZero() {
		gc.HopMax = defaults.HopMax
	}

	return gc
}

type UpstreamGroups map[string][]Upstream

func (c *Upstreams) hasQuicUpstream() bool {
	for _, upstreams := range c.Groups {
		for _, u := range upstreams {
			if u.Net == NetProtocolQuic {
				return true
			}
		}
	}

	return false
}

func (c *Upstreams) validate(logger *logrus.Entry) error {
	defaults := mustDefault[Upstreams]()

	if !c.Timeout.IsAboveZero() {
		logger.Warnf("upstreams.timeout <= 0, setting to %s", defaults.Timeout)
		c.Timeout = defaults.Timeout
	}

	if c.hasQuicUpstream() {
		if !c.QUIC.MaxIdleTimeout.IsAboveZero() {
			logger.Warnf("upstreams.quic.maxIdleTimeout <= 0, setting to %s", defaults.QUIC.MaxIdleTimeout)
			c.QUIC.MaxIdleTimeout = defaults.QUIC.MaxIdleTimeout
		}

		if !c.QUIC.KeepAlivePeriod.IsAboveZero() {
			logger.Warnf("upstreams.quic.keepAlivePeriod <= 0, setting to %s", defaults.QUIC.KeepAlivePeriod)
			c.QUIC.KeepAlivePeriod = defaults.QUIC.KeepAlivePeriod
		}

		if c.QUIC.KeepAlivePeriod.ToDuration() >= c.QUIC.MaxIdleTimeout.ToDuration() {
			logger.Warn("upstreams.quic.keepAlivePeriod >= maxIdleTimeout, keep-alive won't prevent idle timeout")
		}
	}

	return c.validateGroupConfig()
}

func (c *Upstreams) validateGroupConfig() error {
	for group := range c.GroupConfig {
		if c.EffectiveStrategy(group) != UpstreamStrategyTimeHop {
			continue
		}

		raw := c.GroupConfig[group]

		// explicit non-positive values are an error; zero/absent gets the default
		if (raw.HopMin != 0 && !raw.HopMin.IsAboveZero()) || (raw.HopMax != 0 && !raw.HopMax.IsAboveZero()) {
			return fmt.Errorf("upstreams.groupConfig.%s: hopMin and hopMax must be > 0 for strategy time_hop", group)
		}

		gc := c.GroupSettings(group)

		if gc.HopMin.ToDuration() > gc.HopMax.ToDuration() {
			return fmt.Errorf("upstreams.groupConfig.%s: hopMin (%s) must be <= hopMax (%s)",
				group, gc.HopMin, gc.HopMax)
		}
	}

	return nil
}

// IsEnabled implements `config.Configurable`.
func (c *Upstreams) IsEnabled() bool {
	return len(c.Groups) != 0
}

// LogConfig implements `config.Configurable`.
func (c *Upstreams) LogConfig(logger *logrus.Entry) {
	logger.Info("init:")
	log.WithIndent(logger, "  ", c.Init.LogConfig)

	logger.Info("timeout: ", c.Timeout)
	logger.Info("strategy: ", c.Strategy)
	logger.Info("groups:")

	for name, upstreams := range c.Groups {
		logger.Infof("  %s:", name)

		for _, upstream := range upstreams {
			logger.Infof("    - %s", upstream)
		}
	}

	if c.hasQuicUpstream() {
		logger.Info("quic:")
		logger.Info("  maxIdleTimeout: ", c.QUIC.MaxIdleTimeout)
		logger.Info("  keepAlivePeriod: ", c.QUIC.KeepAlivePeriod)
	}
}

// UpstreamGroup represents the config for one group (upstream branch)
type UpstreamGroup struct {
	Upstreams

	Name string // group name
}

// NewUpstreamGroup creates an UpstreamGroup with the given name and upstreams.
//
// The upstreams from `cfg.Groups` are ignored.
func NewUpstreamGroup(name string, cfg Upstreams, upstreams []Upstream) UpstreamGroup {
	group := UpstreamGroup{
		Name:      name,
		Upstreams: cfg,
	}

	group.Groups = UpstreamGroups{name: upstreams}

	return group
}

func (c *UpstreamGroup) GroupUpstreams() []Upstream {
	return c.Groups[c.Name]
}

// IsEnabled implements `config.Configurable`.
func (c *UpstreamGroup) IsEnabled() bool {
	return len(c.GroupUpstreams()) != 0
}

// LogConfig implements `config.Configurable`.
func (c *UpstreamGroup) LogConfig(logger *logrus.Entry) {
	logger.Info("group: ", c.Name)
	logger.Info("upstreams:")

	for _, upstream := range c.GroupUpstreams() {
		logger.Infof("  - %s", upstream)
	}
}
