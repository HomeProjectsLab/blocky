package config

import (
	"fmt"

	"github.com/sirupsen/logrus"
)

// maxCacheExpirationHours bounds the DNSSEC verdict cache lifetime (1 year).
// Larger values risk int64 overflow when converted to time.Duration and would
// pin secure/bogus verdicts across key rollovers.
const maxCacheExpirationHours = 24 * 365

// DNSSEC is the configuration for DNSSEC validation
type DNSSEC struct {
	// Enable DNSSEC validation of DNS responses.
	Validate bool `default:"false" yaml:"validate"`
	// Custom trust anchors (DNSKEY or DS records); empty uses built-in IANA root trust anchors.
	TrustAnchors []string `yaml:"trustAnchors"`
	// Maximum domain label depth for chain of trust validation (DoS protection).
	MaxChainDepth uint `default:"10" yaml:"maxChainDepth"`
	// How long to cache DNSSEC validation results, in hours.
	CacheExpirationHours uint `default:"1"   yaml:"cacheExpirationHours"`
	MaxNSEC3Iterations   uint `default:"150" yaml:"maxNSEC3Iterations"` // RFC 5155 §10.3
	// DoS protection: max upstream queries per validation
	MaxUpstreamQueries uint `default:"30" yaml:"maxUpstreamQueries"`
	// Clock skew tolerance in seconds for signature validation (default: 3600 = 1 hour)
	// Allows validation to succeed even if system clock is off by this amount.
	// Matches Unbound/BIND defaults for real-world deployments (VMs, containers, embedded systems).
	// Per RFC 6781 §4.1.2: Validators should account for clock skew in deployment environments.
	ClockSkewToleranceSec uint `default:"3600" yaml:"clockSkewToleranceSec"`
}

// validate checks bounds on DNSSEC settings.
func (c *DNSSEC) validate() error {
	if c.CacheExpirationHours > maxCacheExpirationHours {
		return fmt.Errorf("dnssec: cacheExpirationHours (%d) must be <= %d",
			c.CacheExpirationHours, maxCacheExpirationHours)
	}

	// Compared against a uint16 iteration count (RFC 5155); larger values
	// truncate to uint16 and would reject every NSEC3 proof.
	if c.MaxNSEC3Iterations > 65535 {
		return fmt.Errorf("dnssec: maxNSEC3Iterations (%d) must be <= 65535", c.MaxNSEC3Iterations)
	}

	return nil
}

// IsEnabled returns true if DNSSEC validation is enabled
func (c *DNSSEC) IsEnabled() bool {
	return c.Validate
}

// LogConfig logs the DNSSEC configuration
func (c *DNSSEC) LogConfig(logger *logrus.Entry) {
	logger.Infof("Validation = %t", c.Validate)

	if c.Validate {
		if len(c.TrustAnchors) > 0 {
			logger.Infof("Custom trust anchors = %d", len(c.TrustAnchors))
		} else {
			logger.Info("Using default root trust anchors")
		}
		logger.Infof("Max chain depth = %d", c.MaxChainDepth)
		logger.Infof("Cache expiration = %d hour(s)", c.CacheExpirationHours)
		logger.Infof("Max NSEC3 iterations = %d", c.MaxNSEC3Iterations)
		logger.Infof("Max upstream queries per validation = %d", c.MaxUpstreamQueries)
		logger.Infof("Clock skew tolerance = %d second(s)", c.ClockSkewToleranceSec)
	}
}
