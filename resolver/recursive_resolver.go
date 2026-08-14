package resolver

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"sync/atomic"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"
	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
	"github.com/zmap/zdns/v2/src/zdns"
)

const (
	recursiveResolverType = "recursive"

	recursiveReason = "RESOLVED (recursive:iterative)"
)

// RecursiveResolver resolves queries iteratively from the root servers using
// zmap/zdns, with DNSSEC validation enabled: bogus answers become SERVFAIL.
//
// If the group has upstreams configured they form a fallback tier (parallel_best):
// on recursion failure (not on clean NOERROR/NXDOMAIN and never on DNSSEC-bogus)
// the query is forwarded to them.
type RecursiveResolver struct {
	configurable[*config.UpstreamGroup]
	typed

	// zcfg is swapped wholesale on cache flush (fresh zdns cache): new lookups pick
	// up the new cache, in-flight lookups keep the old one.
	zcfg     atomic.Pointer[zdns.ResolverConfig]
	fallback Resolver
}

// NewRecursiveResolver creates a new recursive resolver for the given group. If the
// group has upstreams, they are built into an internal parallel_best fallback tier.
func NewRecursiveResolver(
	ctx context.Context, cfg config.UpstreamGroup, bootstrap *Bootstrap,
) (*RecursiveResolver, error) {
	r := RecursiveResolver{
		configurable: withConfig(&cfg),
		typed:        withType(recursiveResolverType),
	}

	r.zcfg.Store(newZdnsConfig())

	if len(cfg.GroupUpstreams()) > 0 {
		fallback, err := NewParallelBestResolver(ctx, cfg, bootstrap)
		if err != nil {
			return nil, fmt.Errorf("can't create fallback resolver for recursive group '%s': %w", cfg.Name, err)
		}

		r.fallback = fallback
	}

	return &r, nil
}

func newZdnsConfig() *zdns.ResolverConfig {
	zcfg := zdns.NewResolverConfig()

	// iterative resolution with DNSSEC validation
	zcfg.DNSSecEnabled = true
	zcfg.ShouldValidateDNSSEC = true

	// zdns logs to the global logrus logger, which blocky doesn't use; keep it quiet
	zcfg.LogLevel = logrus.ErrorLevel

	return zcfg
}

// IsEnabled implements `config.Configurable`.
//
// A recursive group is functional without any upstreams (they are only the
// optional fallback tier), so it is always enabled.
func (r *RecursiveResolver) IsEnabled() bool {
	return true
}

func (r *RecursiveResolver) Name() string {
	return r.String()
}

func (r *RecursiveResolver) String() string {
	if r.fallback == nil {
		return fmt.Sprintf("%s '%s'", recursiveResolverType, r.cfg.Name)
	}

	return fmt.Sprintf("%s '%s' (fallback: %s)", recursiveResolverType, r.cfg.Name, r.fallback)
}

// FlushCaches clears the internal zdns cache. zdns exposes no in-place clear, so
// the whole resolver config is swapped for one with a fresh cache.
func (r *RecursiveResolver) FlushCaches(ctx context.Context) {
	_, logger := r.log(ctx)
	logger.Debug("flushing zdns cache")

	r.zcfg.Store(newZdnsConfig())
}

func (r *RecursiveResolver) Resolve(ctx context.Context, request *model.Request) (*model.Response, error) {
	ctx, logger := r.log(ctx)

	question := request.Req.Question[0]

	// zdns resolvers are not safe for concurrent lookups: create one per lookup,
	// sharing the cache via the config.
	// ponytail: per-lookup InitResolver (allocs + one socket); pool resolvers if profiling shows overhead
	zr, err := zdns.InitResolver(r.zcfg.Load())
	if err != nil {
		return nil, fmt.Errorf("can't init zdns resolver: %w", err)
	}
	defer zr.Close()

	q := zdns.Question{
		Name:  strings.TrimSuffix(question.Name, "."),
		Type:  question.Qtype,
		Class: question.Qclass,
	}

	// NOTE (privacy.queryCaseRandomization): 0x20 case randomization is NOT applied on
	// the recursive egress. zdns builds its own datagram in wireLookupUDP/wireLookupTCP/
	// doDoT/doDoH (zdns/src/zdns/lookup.go) via m.SetQuestion(dotName(q.Name), …); dotName
	// (util.go) only appends a trailing dot and preserves case, and per-query buffer size is
	// hardcoded (m.SetEdns0(1232, dnssec)) — neither is reachable from ResolverConfig. Making
	// them per-query needs a zdns fork of those four funcs: replace dotName(q.Name) with a
	// 0x20-randomized name and verify/normalize the echoed case on the returned msg (the exact
	// randomizeQuestionCase / normalizeResponseCase pair used on the forwarding path in
	// query_case.go). Until that fork lands the toggle is a documented no-op here — the
	// forwarding path (UpstreamResolver) honors it fully, including the recursive group's
	// upstream fallback tier. Static EDNS options (cookie/padding) WOULD be honored via
	// zcfg.EdnsOptions (see newZdnsConfig), but padding plaintext auth queries is as pointless
	// as padding port-53 (padEncryptedRequest declines it too), so none is injected.
	res, _, status, err := zr.IterativeLookup(ctx, &q)

	// A DNSSEC-bogus answer means SERVFAIL, never the fallback tier: forwarding
	// would serve exactly the data validation rejected.
	// (zdns returns the bogus result with status NOERROR and only marks DNSSECResult.)
	if res != nil && res.DNSSECResult != nil && res.DNSSECResult.Status == zdns.DNSSECBogus {
		logger.WithField("reason", res.DNSSECResult.Reason).Debug("DNSSEC validation failed (bogus)")

		return newResponse(request, dns.RcodeServerFailure, model.ResponseTypeRESOLVED,
			"RESOLVED (recursive:iterative, DNSSEC bogus)"), nil
	}

	switch status { //nolint:exhaustive // every other zdns status means "no answer" and takes the fallback path
	case zdns.StatusNoError, zdns.StatusNXDomain:
		// clean answer (incl. NOERROR with empty answer section): never fall back
		return r.buildResponse(logger, request, res, status), nil
	default:
		if r.fallback != nil {
			logger.WithFields(logrus.Fields{"status": status, logrus.ErrorKey: err}).
				Debug("recursion failed, delegating to fallback upstreams")

			return r.resolveViaFallback(ctx, request)
		}

		if err != nil {
			return nil, fmt.Errorf("recursive resolution failed with status %s: %w", status, err)
		}

		return nil, fmt.Errorf("recursive resolution failed with status %s", status)
	}
}

func (r *RecursiveResolver) buildResponse(
	logger *logrus.Entry, request *model.Request, res *zdns.SingleQueryResult, status zdns.Status,
) *model.Response {
	rcode := dns.RcodeSuccess
	if status == zdns.StatusNXDomain {
		rcode = dns.RcodeNameError
	}

	msg := new(dns.Msg)
	msg.SetRcode(request.Req, rcode) // restores request ID and question
	msg.RecursionAvailable = true

	if res != nil {
		msg.Answer = zdnsRecordsToRRs(logger, res.Answers)
		msg.Ns = zdnsRecordsToRRs(logger, res.Authorities)
		msg.Extra = zdnsRecordsToRRs(logger, res.Additionals)

		if res.DNSSECResult != nil && res.DNSSECResult.Status == zdns.DNSSECSecure {
			msg.AuthenticatedData = true
		}
	}

	return &model.Response{Res: msg, RType: model.ResponseTypeRESOLVED, Reason: recursiveReason}
}

func (r *RecursiveResolver) resolveViaFallback(
	ctx context.Context, request *model.Request,
) (*model.Response, error) {
	resp, err := r.fallback.Resolve(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("recursive fallback resolution failed: %w", err)
	}

	// "RESOLVED (1.2.3.4:53)" -> "RESOLVED (recursive-fallback:1.2.3.4:53)"
	inner := strings.TrimSuffix(strings.TrimPrefix(resp.Reason, "RESOLVED ("), ")")
	resp.Reason = fmt.Sprintf("RESOLVED (recursive-fallback:%s)", inner)

	return resp, nil
}

// zdnsRecordsToRRs converts zdns answer records to miekg/dns resource records.
// Records that can't be converted are dropped with a debug log.
func zdnsRecordsToRRs(logger *logrus.Entry, records []any) []dns.RR {
	rrs := make([]dns.RR, 0, len(records))

	for _, record := range records {
		rr, err := zdnsRecordToRR(record)
		if err != nil {
			logger.WithError(err).Debug("dropping record not convertible to dns.RR")

			continue
		}

		rrs = append(rrs, rr)
	}

	return rrs
}

// zdnsRecordToRR converts a single zdns answer (parsed struct) back into a
// miekg/dns RR via the DNS presentation format, which both libraries share.
func zdnsRecordToRR(record any) (dns.RR, error) {
	switch v := record.(type) {
	case zdns.Answer: // A, AAAA, NS, CNAME, DNAME, PTR, TXT, ...
		return newRRFromParts(v, baseAnswerRdata(v))
	case zdns.PrefAnswer: // MX and friends
		return newRRFromParts(v.Answer, fmt.Sprintf("%d %s", v.Preference, dns.Fqdn(v.Answer.Answer)))
	case zdns.SOAAnswer:
		return newRRFromParts(v.Answer, fmt.Sprintf("%s %s %d %d %d %d %d",
			dns.Fqdn(v.Ns), dns.Fqdn(v.Mbox), v.Serial, v.Refresh, v.Retry, v.Expire, v.Minttl))
	case zdns.SRVAnswer:
		return newRRFromParts(v.Answer, fmt.Sprintf("%d %d %d %s", v.Priority, v.Weight, v.Port, dns.Fqdn(v.Target)))
	case zdns.CAAAnswer:
		return newRRFromParts(v.Answer, fmt.Sprintf("%d %s %q", v.Flag, v.Tag, v.Value))
	case zdns.TLSAAnswer:
		return newRRFromParts(v.Answer, fmt.Sprintf("%d %d %d %s", v.CertUsage, v.Selector, v.MatchingType, v.Certificate))
	case zdns.NAPTRAnswer:
		return newRRFromParts(v.Answer, fmt.Sprintf("%d %d %q %q %q %s",
			v.Order, v.Preference, v.Flags, v.Service, v.Regexp, dns.Fqdn(v.Replacement)))
	case zdns.SVCBAnswer: // SVCB + HTTPS
		rdata, err := svcbRdata(v)
		if err != nil {
			return nil, err
		}

		return newRRFromParts(v.Answer, rdata)
	// DNSSEC types: zdns provides conversion to zmap/dns types, whose presentation
	// format miekg/dns parses.
	case zdns.DNSKEYAnswer:
		return dns.NewRR(v.ToVanillaType().String())
	case zdns.DSAnswer:
		return dns.NewRR(v.ToVanillaType().String())
	case zdns.RRSIGAnswer:
		return dns.NewRR(v.ToVanillaType().String())
	case zdns.NSECAnswer:
		return dns.NewRR(v.ToVanillaType().String())
	case zdns.NSEC3Answer:
		return dns.NewRR(v.ToVanillaType().String())
	default:
		// ponytail: exotic RR types (LOC, HIP, ...) and OPT pseudo-records are dropped; add cases when needed
		return nil, fmt.Errorf("unsupported zdns record type %T", record)
	}
}

func newRRFromParts(base zdns.Answer, rdata string) (dns.RR, error) {
	rr, err := dns.NewRR(fmt.Sprintf("%s %d %s %s %s", dns.Fqdn(base.Name), base.TTL, base.Class, base.Type, rdata))
	if err != nil {
		return nil, fmt.Errorf("can't rebuild %s record for '%s': %w", base.Type, base.Name, err)
	}

	return rr, nil
}

// baseAnswerRdata returns the rdata in presentation format for simple answers.
func baseAnswerRdata(v zdns.Answer) string {
	switch v.RrType {
	case dns.TypeTXT:
		// zdns joins the character-strings with newlines and strips quoting
		segments := strings.Split(v.Answer, "\n")
		for i, s := range segments {
			segments[i] = quoteTXTSegment(s)
		}

		return strings.Join(segments, " ")
	case dns.TypeNS, dns.TypeCNAME, dns.TypeDNAME, dns.TypePTR:
		return dns.Fqdn(v.Answer)
	default:
		return v.Answer
	}
}

func quoteTXTSegment(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// svcbRdata rebuilds the presentation rdata of an SVCB/HTTPS record.
func svcbRdata(v zdns.SVCBAnswer) (string, error) {
	parts := []string{fmt.Sprintf("%d %s", v.Priority, dns.Fqdn(v.Target))}

	for key, val := range v.SVCParams {
		switch tv := val.(type) {
		case []string: // alpn, mandatory
			parts = append(parts, key+"="+strings.Join(tv, ","))
		case uint16: // port
			parts = append(parts, fmt.Sprintf("%s=%d", key, tv))
		case bool: // no-default-alpn
			parts = append(parts, key)
		case []net.IP: // ipv4hint, ipv6hint
			ips := make([]string, len(tv))
			for i, ip := range tv {
				ips[i] = ip.String()
			}

			parts = append(parts, key+"="+strings.Join(ips, ","))
		case []byte:
			if key != "ech" {
				return "", fmt.Errorf("unsupported SVCB param '%s'", key)
			}

			parts = append(parts, key+"="+base64.StdEncoding.EncodeToString(tv))
		default:
			// drop the whole RR rather than serve a mangled one
			return "", fmt.Errorf("unsupported SVCB param '%s' (%T)", key, val)
		}
	}

	return strings.Join(parts, " "), nil
}
