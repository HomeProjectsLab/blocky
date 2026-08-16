package config

import (
	"fmt"
	"net"
	"strings"

	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

// CustomDNS custom DNS configuration
type CustomDNS struct {
	RewriterConfig `yaml:",inline"`

	// TTL for DNS records defined in the mapping section (does not apply to zone file records).
	CustomTTL Duration `default:"1h" yaml:"customTTL"`
	// Simple domain-to-IP mappings; multiple IPs per domain separated by commas.
	Mapping CustomDNSMapping `yaml:"mapping"`
	// DNS zone file content for more complex record definitions (A, AAAA, CNAME, TXT, SRV).
	Zone ZoneFileDNS `default:"" yaml:"zone"`
	// If true, queries for types not defined for a domain return empty; if false, they are forwarded upstream.
	FilterUnmappedTypes bool `default:"true" yaml:"filterUnmappedTypes"`
	// Domains answered with NXDOMAIN (name error) instead of a record. Exact match
	// only. Use for signals that require a real NXDOMAIN rather than a 0.0.0.0 block
	// — e.g. Firefox's use-application-dns.net canary, which disables DoH only on
	// NXDOMAIN. Matched before the zone/mapping.
	NXDomains []string `yaml:"nxdomain"`
}

type (
	CustomDNSMapping map[string]CustomDNSEntries
	CustomDNSEntries []dns.RR

	ZoneFileDNS struct {
		RRs        CustomDNSMapping
		configPath string
	}
)

func (z *ZoneFileDNS) UnmarshalYAML(unmarshal func(any) error) error {
	var input string
	if err := unmarshal(&input); err != nil {
		return fmt.Errorf("failed to unmarshal zone file DNS: %w", err)
	}

	result := make(CustomDNSMapping)

	zoneParser := dns.NewZoneParser(strings.NewReader(input), "", z.configPath)
	// $INCLUDE reads arbitrary local files. Only the disk-load path (which sets
	// configPath) may use it; the web/validate pipeline (empty configPath) must
	// not become a local-file probe.
	zoneParser.SetIncludeAllowed(z.configPath != "")

	for {
		zoneRR, ok := zoneParser.Next()

		if !ok {
			if zoneParser.Err() != nil {
				return fmt.Errorf("zone file parsing error: %w", zoneParser.Err())
			}

			// Done
			break
		}

		domain := zoneRR.Header().Name

		if _, ok := result[domain]; !ok {
			result[domain] = make(CustomDNSEntries, 0, 1)
		}

		result[domain] = append(result[domain], zoneRR)
	}

	z.RRs = result

	return nil
}

func (c *CustomDNSEntries) UnmarshalYAML(unmarshal func(any) error) error {
	var input string
	if err := unmarshal(&input); err != nil {
		return fmt.Errorf("failed to unmarshal custom DNS entries: %w", err)
	}

	parts := strings.Split(input, ",")
	result := make(CustomDNSEntries, len(parts))

	for i, part := range parts {
		rr, err := configToRR(strings.TrimSpace(part))
		if err != nil {
			return fmt.Errorf("invalid custom DNS entry '%s': %w", part, err)
		}

		result[i] = rr
	}

	*c = result

	return nil
}

// IsEnabled implements `config.Configurable`.
//
// The JungleBlock Local-DNS UI writes records into `Zone` (and NXDOMAIN entries
// into `NXDomains`), never `Mapping`. Counting only `Mapping` reported a working
// zone-only config as disabled — so LogResolverConfig logged `custom_dns:
// disabled` and LogConfig printed nothing while the resolver was actively
// serving those records, which made local-DNS problems undebuggable from logs.
func (c *CustomDNS) IsEnabled() bool {
	return len(c.Mapping) != 0 || len(c.Zone.RRs) != 0 || len(c.NXDomains) != 0
}

// LogConfig implements `config.Configurable`.
func (c *CustomDNS) LogConfig(logger *logrus.Entry) {
	logger.Debugf("TTL = %s", c.CustomTTL)
	logger.Infof("filterUnmappedTypes = %t", c.FilterUnmappedTypes)

	// With filterUnmappedTypes=false, a query TYPE not defined for a matched name
	// (AAAA, and HTTPS/SVCB which browsers send) is forwarded UPSTREAM instead of
	// answered empty — so an override leaks the real origin's records and the
	// client still reaches the real server. Make that footgun visible.
	if !c.FilterUnmappedTypes {
		logger.Warn("filterUnmappedTypes=false: query types not defined for a custom/override " +
			"domain (AAAA, HTTPS/SVCB, …) are forwarded upstream and can leak the real server")
	}

	logger.Info("mapping:")

	for key, val := range c.Mapping {
		logger.Infof("  %s = %s", key, val)
	}

	logger.Infof("zone records: %d", len(c.Zone.RRs))

	for key, val := range c.Zone.RRs {
		logger.Infof("  %s = %s", key, val)
	}

	if len(c.NXDomains) != 0 {
		logger.Infof("nxdomain = %v", c.NXDomains)
	}
}

func configToRR(ipStr string) (dns.RR, error) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP address '%s'", ipStr)
	}

	if ip.To4() != nil {
		a := new(dns.A)
		a.A = ip

		return a, nil
	}

	aaaa := new(dns.AAAA)
	aaaa.AAAA = ip

	return aaaa, nil
}
