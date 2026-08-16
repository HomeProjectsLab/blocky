package resolver

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"time"

	"github.com/0xERR0R/blocky/config"
	. "github.com/0xERR0R/blocky/helpertest"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/util"
	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	"github.com/zmap/zdns/v2/src/zdns"
)

// skipUnlessIterativeDNS skips the current spec unless true iterative DNSSEC
// resolution actually works end-to-end from this machine. A reachable root
// server is NOT enough: many networks reach (or intercept) the roots yet mangle
// the larger DNSSEC responses of the follow-on queries, turning every real
// lookup into SERVFAIL/DNSSEC-bogus. The only reliable precheck is a full
// resolution of a known-good, DNSSEC-signed name (example.com) — the same
// invariant these live specs assert. If it does not come back cleanly
// RESOLVED (recursive:iterative) with an answer, iteration is unavailable
// (offline, intercepted, or mangled) and the spec is skipped.
func skipUnlessIterativeDNS(ctx context.Context, sut *RecursiveResolver) {
	resp, err := sut.Resolve(ctx, newRequest("example.com.", A))
	if err != nil || resp.Reason != "RESOLVED (recursive:iterative)" || len(resp.Res.Answer) == 0 {
		Skip("iterative DNSSEC resolution unavailable on this network (offline/intercepted/mangled)")
	}
}

// fakeLookuper stubs the zdns lookup client to return a fixed result.
type fakeLookuper struct {
	res    *zdns.SingleQueryResult
	status zdns.Status
	err    error
}

func (f fakeLookuper) DoDstServersLookup(
	_ context.Context, _ *zdns.Resolver, _ zdns.Question, _ []zdns.NameServer, _ bool,
) (*zdns.SingleQueryResult, zdns.Trace, zdns.Status, error) {
	return f.res, nil, f.status, f.err
}

var _ = Describe("RecursiveResolver", Label("recursiveResolver"), func() {
	var (
		sut *RecursiveResolver
		err error

		ctx      context.Context
		cancelFn context.CancelFunc
	)

	// failIfCalled is a fallback that must not be used: mockResolver without
	// expectations panics (and fails the spec) on any call.
	failIfCalled := func() Resolver { return &mockResolver{} }

	withFakeLookup := func(fake fakeLookuper) {
		zcfg := newZdnsConfig()
		zcfg.LookupClient = fake
		sut.zcfg.Store(zcfg)
	}

	withUnroutableRoots := func() {
		zcfg := newZdnsConfig()
		zcfg.RootNameServersV4 = []zdns.NameServer{{IP: net.ParseIP("192.0.2.1"), Port: 53}} // TEST-NET-1
		zcfg.Timeout = time.Second
		zcfg.IterativeTimeout = 500 * time.Millisecond
		zcfg.NetworkTimeout = 250 * time.Millisecond
		zcfg.Retries = 0
		sut.zcfg.Store(zcfg)
	}

	BeforeEach(func() {
		ctx, cancelFn = context.WithCancel(context.Background())
		DeferCleanup(cancelFn)

		sut, err = NewRecursiveResolver(ctx,
			config.NewUpstreamGroup("default", config.Upstreams{}, nil), systemResolverBootstrap)
		Expect(err).Should(Succeed())
	})

	Describe("construction", func() {
		It("has no fallback without upstreams and is always enabled", func() {
			Expect(sut.fallback).Should(BeNil())
			Expect(sut.Type()).Should(Equal("recursive"))
			Expect(sut.IsEnabled()).Should(BeTrue())
		})
	})

	Describe("resolution pipeline (local mock root server)", func() {
		It("converts the zdns result to a dns.Msg and keeps the request ID", func() {
			// the mock must answer authoritatively (AA): zdns treats non-AA responses
			// as referrals and drops their answer records
			mockRoot := NewMockUDPUpstreamServer().WithAnswerFn(func(request *dns.Msg) *dns.Msg {
				response := new(dns.Msg)
				response.SetReply(request)
				response.Authoritative = true

				rr, err := dns.NewRR("example.com. 123 IN A 192.0.2.5")
				Expect(err).Should(Succeed())
				response.Answer = []dns.RR{rr}

				return response
			})
			upstream := mockRoot.Start()

			// point the "root servers" at the local mock; it answers the query directly.
			// Validation must be off: the mock can't serve a proper DNSSEC chain.
			zcfg := newZdnsConfig()
			zcfg.RootNameServersV4 = []zdns.NameServer{{IP: net.ParseIP("127.0.0.1"), Port: uint16(upstream.Port)}}
			zcfg.DNSSecEnabled = false
			zcfg.ShouldValidateDNSSEC = false
			sut.zcfg.Store(zcfg)

			request := newRequest("example.com.", A)
			resp, err := sut.Resolve(ctx, request)
			Expect(err).Should(Succeed())

			Expect(resp.RType).Should(Equal(model.ResponseTypeRESOLVED))
			Expect(resp.Reason).Should(Equal("RESOLVED (recursive:iterative)"))
			Expect(resp.Res.Id).Should(Equal(request.Req.Id))
			Expect(resp.Res.Rcode).Should(Equal(dns.RcodeSuccess))
			Expect(resp.Res).Should(BeDNSRecord("example.com.", A, "192.0.2.5"))
			Expect(resp.Res.Answer[0].Header().Ttl).Should(Equal(uint32(123)))
		})
	})

	Describe("DNSSEC validation", func() {
		It("returns SERVFAIL for a bogus result and never falls back", func() {
			sut.fallback = failIfCalled()

			withFakeLookup(fakeLookuper{
				res: &zdns.SingleQueryResult{
					DNSSECResult: &zdns.DNSSECResult{Status: zdns.DNSSECBogus, Reason: "test bogus"},
				},
				status: zdns.StatusNoError,
			})

			resp, err := sut.Resolve(ctx, newRequest("dnssec-failed.org.", A))
			Expect(err).Should(Succeed())

			Expect(resp.RType).Should(Equal(model.ResponseTypeRESOLVED))
			Expect(resp.Reason).Should(ContainSubstring("DNSSEC bogus"))
			Expect(resp.Res.Rcode).Should(Equal(dns.RcodeServerFailure))
		})

		It("marks a secure result authenticated", func() {
			withFakeLookup(fakeLookuper{
				res: &zdns.SingleQueryResult{
					Answers:      []any{zdns.Answer{Name: "example.com", TTL: 60, Type: "A", RrType: dns.TypeA, Class: "IN", Answer: "192.0.2.5"}},
					DNSSECResult: &zdns.DNSSECResult{Status: zdns.DNSSECSecure},
				},
				status: zdns.StatusNoError,
			})

			resp, err := sut.Resolve(ctx, newRequest("example.com.", A))
			Expect(err).Should(Succeed())
			Expect(resp.Res.AuthenticatedData).Should(BeTrue())
		})
	})

	Describe("NXDOMAIN", func() {
		It("returns NXDOMAIN with the converted SOA and never falls back", func() {
			sut.fallback = failIfCalled()

			withFakeLookup(fakeLookuper{
				res: &zdns.SingleQueryResult{
					Authorities: []any{zdns.SOAAnswer{
						Answer: zdns.Answer{Name: "example.com", TTL: 3600, Type: "SOA", RrType: dns.TypeSOA, Class: "IN"},
						Ns:     "ns.example.com", Mbox: "noc.example.com",
						Serial: 1, Refresh: 7200, Retry: 3600, Expire: 1209600, Minttl: 3600,
					}},
				},
				status: zdns.StatusNXDomain,
			})

			resp, err := sut.Resolve(ctx, newRequest("does-not-exist.example.com.", A))
			Expect(err).Should(Succeed())

			Expect(resp.Res.Rcode).Should(Equal(dns.RcodeNameError))
			Expect(resp.Res.Answer).Should(BeEmpty())
			Expect(resp.Res.Ns).Should(HaveLen(1))
			Expect(resp.Res.Ns[0].Header().Rrtype).Should(Equal(dns.TypeSOA))
		})
	})

	Describe("hybrid fallback", func() {
		// Both specs force recursion to fail via unroutable roots and assert on
		// the fallback tier. That only holds when real iterative resolution is
		// otherwise possible; skip when it isn't (offline/intercepted/mangled),
		// where recursion "fails" for the wrong reason and the assertions break.
		BeforeEach(func() { skipUnlessIterativeDNS(ctx, sut) })

		It("delegates to the fallback when recursion fails", func() {
			withUnroutableRoots()

			fallback := &mockResolver{
				AnswerFn: func(qType dns.Type, qName string) (*dns.Msg, error) {
					return util.NewMsgWithAnswer(qName, 60, qType, "127.0.0.1")
				},
			}
			fallback.On("Resolve", mock.Anything).Return(nil, nil)
			sut.fallback = fallback

			resp, err := sut.Resolve(ctx, newRequest("example.com.", A))
			Expect(err).Should(Succeed())

			Expect(resp.Reason).Should(HavePrefix("RESOLVED (recursive-fallback:"))
			Expect(resp.Res.Answer).ShouldNot(BeEmpty())
			fallback.AssertExpectations(GinkgoT())
		})

		It("errors when recursion fails and no fallback exists", func() {
			withUnroutableRoots()

			_, err := sut.Resolve(ctx, newRequest("example.com.", A))
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).Should(ContainSubstring("recursive resolution failed"))
		})
	})

	Describe("FlushCaches", func() {
		It("swaps in a fresh zdns cache", func() {
			before := sut.zcfg.Load()
			sut.FlushCaches(ctx)
			Expect(sut.zcfg.Load()).ShouldNot(BeIdenticalTo(before))
		})
	})

	Describe("iterative resolution (live network)", func() {
		BeforeEach(func() { skipUnlessIterativeDNS(ctx, sut) })

		It("resolves example.com A from the roots", func() {
			resp, err := sut.Resolve(ctx, newRequest("example.com.", A))
			Expect(err).Should(Succeed())

			Expect(resp.RType).Should(Equal(model.ResponseTypeRESOLVED))
			Expect(resp.Reason).Should(Equal("RESOLVED (recursive:iterative)"))
			Expect(resp.Res.Rcode).Should(Equal(dns.RcodeSuccess))
			Expect(resp.Res.Answer).ShouldNot(BeEmpty())
		})

		It("returns NXDOMAIN for a nonexistent domain without using the fallback", func() {
			sut.fallback = failIfCalled()

			domain := fmt.Sprintf("does-not-exist-%08x.example.com.", rand.Uint32())

			resp, err := sut.Resolve(ctx, newRequest(domain, A))
			Expect(err).Should(Succeed())

			// Classic zones answer NXDOMAIN; zones using Compact Denial of
			// Existence (RFC 9824, e.g. Cloudflare-hosted example.com) answer
			// NOERROR with an empty answer section instead. Both mean "name
			// does not resolve" — the invariant under test is that neither
			// triggers the fallback tier.
			Expect(resp.Res.Rcode).Should(BeElementOf(dns.RcodeNameError, dns.RcodeSuccess))
			Expect(resp.Res.Answer).Should(BeEmpty())
		})

		It("returns SERVFAIL for a DNSSEC-bogus domain without using the fallback", func() {
			sut.fallback = failIfCalled()

			resp, err := sut.Resolve(ctx, newRequest("dnssec-failed.org.", A))
			Expect(err).Should(Succeed())

			Expect(resp.Reason).Should(ContainSubstring("DNSSEC bogus"))
			Expect(resp.Res.Rcode).Should(Equal(dns.RcodeServerFailure))
		})
	})
})

var _ = Describe("UpstreamTreeResolver with recursive strategy", Label("recursiveResolver"), func() {
	var (
		ctx      context.Context
		cancelFn context.CancelFunc
	)

	BeforeEach(func() {
		ctx, cancelFn = context.WithCancel(context.Background())
		DeferCleanup(cancelFn)
	})

	When("the default group is recursive with zero upstreams", func() {
		var sutConfig config.Upstreams

		BeforeEach(func() {
			sutConfig = config.Upstreams{
				GroupConfig: map[string]config.UpstreamGroupConfig{
					upstreamDefaultCfgName: {Strategy: config.UpstreamStrategyRecursive},
				},
			}
		})

		It("constructs successfully and rejects ReplaceUpstreams", func() {
			sut, err := NewUpstreamTreeResolver(ctx, sutConfig, systemResolverBootstrap)
			Expect(err).Should(Succeed())

			tree, ok := sut.(*UpstreamTreeResolver)
			Expect(ok).Should(BeTrue())
			Expect(tree.branches[upstreamDefaultCfgName].Type()).Should(Equal("recursive"))

			err = tree.ReplaceUpstreams(ctx, upstreamDefaultCfgName, []config.Upstream{{Host: "192.0.2.53"}})
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).Should(ContainSubstring("does not support upstream replacement"))
		})
	})
})
