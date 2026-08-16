package dnssec

// This file contains NSEC-based denial of existence validation per RFC 4035 §5.4.

import (
	"slices"
	"strings"

	"github.com/0xERR0R/blocky/util"

	"github.com/miekg/dns"
)

// validateNSECDenialOfExistence validates NSEC-based denial of existence per RFC 4035 §5.4
func (v *Validator) validateNSECDenialOfExistence(response *dns.Msg, question dns.Question) ValidationResult {
	nsecRecords := extractNSECRecords(response.Ns)
	if len(nsecRecords) == 0 {
		return ValidationResultInsecure
	}

	if response.Rcode == dns.RcodeNameError {
		return v.validateNSECNXDOMAIN(nsecRecords, question.Name)
	}

	return v.validateNSECNODATA(nsecRecords, question.Name, question.Qtype)
}

// extractNSECRecords extracts all NSEC records from a slice of RRs
func extractNSECRecords(rrs []dns.RR) []*dns.NSEC {
	return util.ExtractRecordsFromSlice[*dns.NSEC](rrs)
}

// validateNSECNXDOMAIN validates NSEC proof for NXDOMAIN
func (v *Validator) validateNSECNXDOMAIN(nsecRecords []*dns.NSEC, qname string) ValidationResult {
	qname = dns.Fqdn(qname)

	// NXDOMAIN: Need to prove the name doesn't exist
	// Find NSEC that covers the query name
	var covering *dns.NSEC

	for _, nsec := range nsecRecords {
		if v.nsecCoversName(nsec, qname) {
			v.logger.Debugf("NSEC covers NXDOMAIN for %s: %s -> %s", qname, nsec.Header().Name, nsec.NextDomain)

			covering = nsec

			break
		}
	}

	if covering == nil {
		v.logger.Warnf("No NSEC record covers NXDOMAIN for %s", qname)

		return ValidationResultBogus
	}

	// RFC 4035 §3.1.3.2/§5.4: NXDOMAIN also requires proof that no wildcard could have
	// synthesized an answer: an authenticated NSEC covering `*.<closest encloser>`
	// (mirrors the NSEC3 NXDOMAIN wildcard check).
	wildcardName := dns.Fqdn("*." + strings.TrimSuffix(nsecClosestEncloser(covering, qname), "."))

	for _, nsec := range nsecRecords {
		if v.nsecCoversName(nsec, wildcardName) {
			v.logger.Debugf("NSEC covers wildcard %s for NXDOMAIN %s", wildcardName, qname)

			return ValidationResultSecure
		}
	}

	v.logger.Warnf("No NSEC record covers wildcard %s for NXDOMAIN %s", wildcardName, qname)

	return ValidationResultBogus
}

// nsecClosestEncloser derives the closest encloser of qname from the NSEC record
// covering it: the owner and next domain both exist in the zone and bracket qname,
// so the closest (longest) existing ancestor of qname is its longest common suffix,
// in whole labels, with either of them. Returns a FQDN ("." for the root).
func nsecClosestEncloser(nsec *dns.NSEC, qname string) string {
	qname = dns.CanonicalName(qname)

	common := dns.CompareDomainName(qname, dns.CanonicalName(nsec.Header().Name))
	if n := dns.CompareDomainName(qname, dns.CanonicalName(nsec.NextDomain)); n > common {
		common = n
	}

	if common == 0 {
		return "."
	}

	labelIndexes := dns.Split(qname)

	return qname[labelIndexes[len(labelIndexes)-common]:]
}

// validateNSECNODATA validates NSEC proof for NODATA
func (v *Validator) validateNSECNODATA(nsecRecords []*dns.NSEC, qname string, qtype uint16) ValidationResult {
	qname = dns.Fqdn(qname)

	// NODATA: Need NSEC at the name proving type doesn't exist
	for _, nsec := range nsecRecords {
		nsecName := dns.Fqdn(nsec.Header().Name)
		if nsecName == qname {
			// NSEC matches the query name - check if it proves type doesn't exist
			if !v.nsecHasType(nsec, qtype) {
				v.logger.Debugf("NSEC proves NODATA for %s type %d", qname, qtype)

				return ValidationResultSecure
			}
			// Type exists according to NSEC - this is bogus
			v.logger.Warnf("NSEC at %s claims type %d exists but no answer returned", qname, qtype)

			return ValidationResultBogus
		}
	}

	// RFC 4035 §3.1.3.4: wildcard NODATA — qname itself doesn't exist (proven by a
	// covering NSEC) but was synthesized from a wildcard whose NSEC lacks the qtype
	// (mirrors checkWildcardNSEC3Match and the NXDOMAIN wildcard path above).
	for _, nsec := range nsecRecords {
		if !v.nsecCoversName(nsec, qname) {
			continue
		}

		wildcardName := dns.CanonicalName("*." + strings.TrimSuffix(nsecClosestEncloser(nsec, qname), "."))

		for _, wc := range nsecRecords {
			if dns.CanonicalName(wc.Header().Name) != wildcardName {
				continue
			}

			if v.nsecHasType(wc, qtype) {
				v.logger.Warnf("NSEC at wildcard %s claims type %d exists but no answer returned", wildcardName, qtype)

				return ValidationResultBogus
			}

			v.logger.Debugf("NSEC proves wildcard NODATA for %s type %d via %s", qname, qtype, wildcardName)

			return ValidationResultSecure
		}
	}

	v.logger.Warnf("No matching NSEC record found for NODATA proof: %s", qname)

	return ValidationResultBogus
}

// canonicalNameCompare compares two DNS names using RFC 4034 §6.1 canonical ordering.
// Canonical ordering compares labels from the rightmost (root) label first.
// If all shared labels match, the shorter name (fewer labels) comes first.
// Both names are lowercased before comparison.
//
// Returns a negative value if a < b, 0 if a == b, a positive value if a > b.
func canonicalNameCompare(a, b string) int {
	a = strings.TrimSuffix(strings.ToLower(dns.Fqdn(a)), ".")
	b = strings.TrimSuffix(strings.ToLower(dns.Fqdn(b)), ".")

	labelsA := strings.Split(a, ".")
	labelsB := strings.Split(b, ".")

	// Compare from rightmost label
	idxA := len(labelsA) - 1
	idxB := len(labelsB) - 1

	for idxA >= 0 && idxB >= 0 {
		if cmp := strings.Compare(labelsA[idxA], labelsB[idxB]); cmp != 0 {
			return cmp
		}

		idxA--
		idxB--
	}

	// All compared labels matched; fewer labels sorts first
	return len(labelsA) - len(labelsB)
}

// nsecCoversName checks if an NSEC record covers a given name (for NXDOMAIN proof)
// Per RFC 4034 §4.1: NSEC RR covers names between owner name and next domain name
// Uses RFC 4034 §6.1 canonical DNS name ordering (label-by-label, right to left).
func (v *Validator) nsecCoversName(nsec *dns.NSEC, name string) bool {
	owner := dns.CanonicalName(nsec.Header().Name)
	next := dns.CanonicalName(nsec.NextDomain)
	name = dns.CanonicalName(name)

	// If owner < name < next, then NSEC covers the name
	// Handle wrap-around at end of zone (when next < owner)
	if canonicalNameCompare(next, owner) > 0 {
		// Normal case: owner < next
		return canonicalNameCompare(name, owner) > 0 && canonicalNameCompare(name, next) < 0
	}
	// Wrap-around case: next < owner (covers names from owner to end and start to next)
	return canonicalNameCompare(name, owner) > 0 || canonicalNameCompare(name, next) < 0
}

// nsecHasType checks if an NSEC record claims a given type exists
func (v *Validator) nsecHasType(nsec *dns.NSEC, qtype uint16) bool {
	return slices.Contains(nsec.TypeBitMap, qtype)
}
