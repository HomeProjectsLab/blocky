package resolver

import (
	crand "crypto/rand"
	"errors"
	"strings"

	"github.com/miekg/dns"
)

// errCaseMismatch is returned when an upstream response fails the DNS 0x20 case
// check: the echoed question name matches the query case-insensitively but not
// exactly, which is the signature of an off-path spoof (a real answer echoes the
// randomized case verbatim). It is treated as a retryable failure so the resolver
// moves on to the next upstream IP.
var errCaseMismatch = errors.New("0x20 case mismatch in upstream response (possible spoofing)")

// randomizeQuestionCase returns a copy of msg with DNS 0x20 case randomization
// (draft-vixie-dnsext-dns0x20) applied to every question name: each ASCII letter
// is independently upper/lower-cased from a CSPRNG. The input msg — the shared
// request.Req — is never mutated (same contract as udpRequestWithBufferFloor).
func randomizeQuestionCase(msg *dns.Msg) *dns.Msg {
	clone := msg.Copy()
	for i := range clone.Question {
		clone.Question[i].Name = randomizeCase(clone.Question[i].Name)
	}

	return clone
}

// randomizeCase flips the case of each ASCII letter in name based on a random bit.
// Non-letters (digits, dots, hyphens, escaped bytes) are left untouched.
func randomizeCase(name string) string {
	b := []byte(name)

	bits := make([]byte, len(b))
	// crypto/rand.Read never returns a short read / error on supported platforms.
	_, _ = crand.Read(bits)

	for i, c := range b {
		lower := c | 0x20 // fold to lower to test for a letter
		if lower < 'a' || lower > 'z' {
			continue
		}

		if bits[i]&1 == 1 {
			b[i] = lower // 'a'..'z'
		} else {
			b[i] = c &^ 0x20 // 'A'..'Z'
		}
	}

	return string(b)
}

// normalizeResponseCase restores the client's canonical question name on resp and
// on any RR owner name that echoed it, then reports whether the upstream correctly
// echoed the randomized 0x20 case. randomized is the question name we put on the
// wire; canonical is what the client asked for.
//
// A response with no question section (common on REFUSED and other error rcodes)
// can't be case-checked, so it passes — matching responseMatchesRequest's leniency.
func normalizeResponseCase(resp *dns.Msg, randomized, canonical string) (echoedOK bool) {
	echoedOK = true

	for i := range resp.Question {
		name := resp.Question[i].Name
		if strings.EqualFold(name, randomized) {
			if name != randomized {
				echoedOK = false // right name, wrong case => not a genuine echo
			}

			resp.Question[i].Name = canonical
		}
	}

	// Answer/authority/additional RRs that echo the queried owner name (the common
	// leading RR, and CNAME chains rooted at it) are normalized back to the client's
	// case so downstream cache/clients see exactly what they asked for.
	for _, section := range [][]dns.RR{resp.Answer, resp.Ns, resp.Extra} {
		for _, rr := range section {
			hdr := rr.Header()
			if strings.EqualFold(hdr.Name, canonical) {
				hdr.Name = canonical
			}
		}
	}

	return echoedOK
}
