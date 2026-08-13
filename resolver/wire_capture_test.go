package resolver

import (
	"context"
	"strings"
	"sync"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/util"

	. "github.com/0xERR0R/blocky/helpertest"
	. "github.com/0xERR0R/blocky/model"

	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// captureUpstream is a mock plain-DNS (tcp+udp) upstream that records the exact
// *dns.Msg it receives on the wire, so a test can assert what randomness actually
// reached the datagram (vs. what was merely stamped on model.Request). It wraps the
// shared MockUDPUpstreamServer; harden agents reuse it to lock in fidelity.
type captureUpstream struct {
	mu       sync.Mutex
	received []*dns.Msg
	upstream config.Upstream
}

// newCaptureUpstream starts a UDP mock upstream that answers a fixed A record and
// records every received query. Cleanup is registered via the mock server.
func newCaptureUpstream() *captureUpstream {
	c := &captureUpstream{}
	srv := NewMockUDPUpstreamServer().WithAnswerFn(func(request *dns.Msg) *dns.Msg {
		c.mu.Lock()
		c.received = append(c.received, request.Copy())
		c.mu.Unlock()

		return rrAnswerFn("example.com. 123 IN A 1.2.3.4")(request)
	})
	c.upstream = srv.Start()

	return c
}

// last returns the most recently received query (as seen on the wire).
func (c *captureUpstream) last() *dns.Msg {
	c.mu.Lock()
	defer c.mu.Unlock()

	Expect(c.received).NotTo(BeEmpty(), "upstream received no query")

	return c.received[len(c.received)-1]
}

// resolverFor builds a plain tcp+udp UpstreamResolver aimed at the capture upstream.
func (c *captureUpstream) resolverFor() *UpstreamResolver {
	cfg := newUpstreamConfig(c.upstream, defaultUpstreamsConfig)

	return newUpstreamResolverUnchecked(cfg, systemResolverBootstrap)
}

// resolverForCaseRandomized is resolverFor with privacy.queryCaseRandomization on.
func (c *captureUpstream) resolverForCaseRandomized() *UpstreamResolver {
	cfg := newUpstreamConfig(c.upstream, defaultUpstreamsConfig)
	cfg.QueryCaseRandomization = true

	return newUpstreamResolverUnchecked(cfg, systemResolverBootstrap)
}

// optCodes returns the wire order of EDNS0 option codes on the received query.
func optCodes(msg *dns.Msg) []uint16 {
	opt := msg.IsEdns0()
	if opt == nil {
		return nil
	}

	codes := make([]uint16, len(opt.Option))
	for i, o := range opt.Option {
		codes[i] = o.Option()
	}

	return codes
}

// This suite DOCUMENTS the wire-level randomness reaching a plain-DNS upstream
// today. Assertions that show randomness stripped/normalized are the finding, not
// a bug in the test — the harden agents flip them once the gap is closed.
var _ = Describe("Wire-level egress capture", func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
		cap    *captureUpstream
	)

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)

		cap = newCaptureUpstream()
	})

	Context("real forwarded query (mixed case + client EDNS cookie)", func() {
		It("shows what survives to the datagram", func() {
			req := newRequest("eXaMpLe.CoM.", A)

			// client-supplied EDNS: small buffer, DO clear, a cookie option
			opt := new(dns.OPT)
			opt.Hdr.Name = "."
			opt.Hdr.Rrtype = dns.TypeOPT
			opt.SetUDPSize(512)
			opt.Option = append(opt.Option, &dns.EDNS0_COOKIE{
				Code: dns.EDNS0COOKIE, Cookie: "0102030405060708",
			})
			req.Req.Extra = append(req.Req.Extra, opt)

			_, err := cap.resolverFor().Resolve(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			got := cap.last()

			By("QNAME case: forwarded verbatim, NO 0x20 added by blocky")
			Expect(got.Question[0].Name).To(Equal("eXaMpLe.CoM."))

			By("EDNS OPT present")
			gotOpt := got.IsEdns0()
			Expect(gotOpt).NotTo(BeNil())

			By("UDP buffer floor RAISED to 1232 (client's 512 overwritten)")
			Expect(gotOpt.UDPSize()).To(Equal(uint16(upstreamUDPBufferFloor)))

			By("client's cookie option IS preserved on the wire")
			Expect(optCodes(got)).To(ContainElement(uint16(dns.EDNS0COOKIE)))
		})
	})

	Context("decoy query (matched fingerprint via applyFingerprint shape)", func() {
		It("shows what survives to the datagram", func() {
			// mimic decoy/engine.go resolveOne: fresh msg, 0x20 on the qname,
			// applyFingerprint => SetEdns0(sampledSize, DO) with NO options.
			const sampledUDPSize = 512

			msg := util.NewMsgWithQuestion(dns.Fqdn("gooGLE.com"), A)
			msg.SetEdns0(sampledUDPSize, true) // fp.EDNSUDPSize, fp.DO — no cookie/opt codes

			req := &Request{Req: msg, Protocol: RequestProtocolUDP, Bypass: true, Decoy: true}

			_, err := cap.resolverFor().Resolve(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			got := cap.last()

			By("decoy 0x20 case SURVIVES to the wire (forwarded verbatim)")
			Expect(got.Question[0].Name).To(Equal("gooGLE.com."))

			gotOpt := got.IsEdns0()
			Expect(gotOpt).NotTo(BeNil())

			By("sampled buffer size 512 OVERWRITTEN to 1232 — fidelity lost on UDP")
			Expect(gotOpt.UDPSize()).To(Equal(uint16(upstreamUDPBufferFloor)))

			By("DO bit survives")
			Expect(gotOpt.Do()).To(BeTrue())

			By("NO option codes reproduced (applyFingerprint sets none) — no cookie, no opt-code list")
			Expect(optCodes(got)).To(BeEmpty())
		})
	})

	// TASK X hardening: with privacy.queryCaseRandomization ON, the outgoing datagram
	// carries a 0x20-randomized name while the client still gets its canonical case back.
	Context("0x20 case randomization ON (forwarding path)", func() {
		// long, all-letter label so an all-same-case roll is astronomically unlikely (~2^-24)
		const canonical = "abcdefghijklmnopqrstuvwx.example.com."

		It("sends a mixed-case name on the wire but returns canonical case to the client", func() {
			req := newRequest(canonical, A)

			resp, err := cap.resolverForCaseRandomized().Resolve(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			got := cap.last()

			By("wire name is case-randomized: same letters, different case from canonical")
			Expect(got.Question[0].Name).NotTo(Equal(canonical))
			Expect(strings.EqualFold(got.Question[0].Name, canonical)).To(BeTrue())

			By("client-facing answer question is normalized back to the exact case asked")
			Expect(resp.Res.Question[0].Name).To(Equal(canonical))

			By("the shared request.Req was never mutated (still canonical)")
			Expect(req.Req.Question[0].Name).To(Equal(canonical))
		})

		It("leaves the name verbatim when the toggle is OFF (default)", func() {
			req := newRequest(canonical, A)

			_, err := cap.resolverFor().Resolve(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			Expect(cap.last().Question[0].Name).To(Equal(canonical))
		})
	})

	// TASK X: prove udpRequestWithBufferFloor keeps the floor a MINIMUM (not a forced
	// value) and never clobbers an explicitly-set OPT's DO bit or option codes.
	Context("EDNS preservation on the plain-UDP egress", func() {
		newReqWithOpt := func(name string, udpSize uint16) *Request {
			req := newRequest(name, A)
			opt := new(dns.OPT)
			opt.Hdr.Name = "."
			opt.Hdr.Rrtype = dns.TypeOPT
			opt.SetUDPSize(udpSize)
			opt.SetDo(true)
			opt.Option = append(opt.Option, &dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: "0102030405060708"})
			req.Req.Extra = append(req.Req.Extra, opt)

			return req
		}

		It("respects an explicit buffer size ABOVE the floor (not lowered) and keeps DO + options", func() {
			_, err := cap.resolverFor().Resolve(ctx, newReqWithOpt("example.com.", 4096))
			Expect(err).NotTo(HaveOccurred())

			got := cap.last().IsEdns0()
			Expect(got).NotTo(BeNil())
			Expect(got.UDPSize()).To(Equal(uint16(4096)), "size above floor must be kept, not forced to 1232")
			Expect(got.Do()).To(BeTrue())
			Expect(optCodes(cap.last())).To(ContainElement(uint16(dns.EDNS0COOKIE)))
		})

		It("raises a sub-floor buffer size to the 1232 floor while keeping DO + options", func() {
			_, err := cap.resolverFor().Resolve(ctx, newReqWithOpt("example.com.", 512))
			Expect(err).NotTo(HaveOccurred())

			got := cap.last().IsEdns0()
			Expect(got).NotTo(BeNil())
			Expect(got.UDPSize()).To(Equal(uint16(upstreamUDPBufferFloor)))
			Expect(got.Do()).To(BeTrue())
			Expect(optCodes(cap.last())).To(ContainElement(uint16(dns.EDNS0COOKIE)))
		})
	})
})

// Unit-level cover for the 0x20 anti-spoof branch that the well-behaved mock can't
// exercise (the mock echoes case verbatim via SetReply): an upstream that answers
// with the right name but wrong case is rejected.
var _ = Describe("0x20 response case handling", func() {
	It("flags an echo whose case doesn't match (spoof signature) and normalizes the name", func() {
		resp := new(dns.Msg)
		resp.SetQuestion("ExAmPlE.com.", dns.TypeA) // wrong case vs what we sent

		ok := normalizeResponseCase(resp, "eXaMpLe.CoM.", "example.com.")

		Expect(ok).To(BeFalse())
		Expect(resp.Question[0].Name).To(Equal("example.com."), "still normalized to canonical")
	})

	It("accepts a verbatim echo and normalizes answer RR owner names", func() {
		resp := new(dns.Msg)
		resp.SetQuestion("eXaMpLe.CoM.", dns.TypeA)
		rr, _ := dns.NewRR("eXaMpLe.CoM. 300 IN A 1.2.3.4")
		resp.Answer = append(resp.Answer, rr)

		ok := normalizeResponseCase(resp, "eXaMpLe.CoM.", "example.com.")

		Expect(ok).To(BeTrue())
		Expect(resp.Question[0].Name).To(Equal("example.com."))
		Expect(resp.Answer[0].Header().Name).To(Equal("example.com."))
	})
})
