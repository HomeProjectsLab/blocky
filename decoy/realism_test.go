//go:build !mips && !mipsle && !mips64 && !mips64le && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package decoy

import (
	"context"
	"strings"
	"sync"

	"github.com/miekg/dns"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/querylog"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Per-query realism (task 2)", func() {
	var cfg config.DecoyConfig

	BeforeEach(func() {
		var e error
		cfg, e = config.WithDefaults[config.DecoyConfig]()
		Expect(e).Should(Succeed())
		cfg.Enable = true
	})

	// captureEng returns an engine whose resolve func records every request.
	captureEng := func(src Source) (*Engine, func() []*model.Request) {
		var mu sync.Mutex
		var captured []*model.Request
		eng := NewEngine(cfg, src, func(_ context.Context, req *model.Request) (*model.Response, error) {
			mu.Lock()
			captured = append(captured, req)
			mu.Unlock()

			return &model.Response{Res: new(dns.Msg)}, nil
		})

		return eng, func() []*model.Request {
			mu.Lock()
			defer mu.Unlock()

			return append([]*model.Request(nil), captured...)
		}
	}

	Describe("device background chatter (#3)", func() {
		It("draws from the embedded set incl. a PTR/in-addr.arpa lookup", func() {
			eng := NewEngine(cfg, &mockSource{}, nil)

			sawPTR, sawHost := false, false
			for i := 0; i < 500; i++ {
				q := eng.chatterQuery()
				if q.qtype == dns.TypePTR {
					sawPTR = true
					Expect(q.name).Should(HaveSuffix(".in-addr.arpa."))
				} else {
					sawHost = true
					Expect(deviceChatter).Should(ContainElement(q.name))
				}
			}
			Expect(sawPTR).Should(BeTrue(), "expected at least one PTR reverse lookup")
			Expect(sawHost).Should(BeTrue(), "expected at least one embedded chatter host")
		})

		It("emits a chatter query through the emit path when ChatterPct=100", func() {
			cfg.MissChaffPct = 0
			cfg.ChatterPct = 100
			cfg.PersonaAttribution = false
			eng, snapshot := captureEng(&mockSource{})

			eng.emit(context.Background())

			reqs := snapshot()
			Expect(reqs).Should(HaveLen(1))
			name := strings.TrimSuffix(reqs[0].Req.Question[0].Name, ".")
			isChatter := reqs[0].Req.Question[0].Qtype == dns.TypePTR
			for _, h := range deviceChatter {
				if h == name {
					isChatter = true
				}
			}
			Expect(isChatter).Should(BeTrue())
		})
	})

	Describe("qtype diversity (#4)", func() {
		It("mixes in a non-A/AAAA/HTTPS type over many draws", func() {
			eng := NewEngine(cfg, nil, nil)

			seen := map[uint16]struct{}{}
			for i := 0; i < 5000; i++ {
				seen[eng.realQtype()] = struct{}{}
			}

			_, txt := seen[dns.TypeTXT]
			_, mx := seen[dns.TypeMX]
			_, ns := seen[dns.TypeNS]
			_, srv := seen[dns.TypeSRV]
			Expect(txt || mx || ns || srv).Should(BeTrue(), "expected a TXT/MX/NS/SRV over 5000 draws")
		})
	})

	Describe("failure realism (#4)", func() {
		It("yields a likely-NXDOMAIN name under a real TLD", func() {
			eng := NewEngine(cfg, nil, nil)

			for i := 0; i < 200; i++ {
				name := eng.deadName()
				label, tld, ok := strings.Cut(name, ".")
				Expect(ok).Should(BeTrue())
				Expect(label).ShouldNot(BeEmpty())
				Expect(deadTLDs).Should(ContainElement(tld))
			}
		})
	})

	Describe("transport diversity (#4)", func() {
		It("stamps TCP on the request when TCPPct=100 and UDP when 0", func() {
			cfg.PersonaAttribution = false
			cfg.TCPPct = 100
			eng, snapshot := captureEng(&mockSource{})
			eng.resolveOne(context.Background(), decoyQuery{name: "tcp.example", qtype: dns.TypeA})
			Expect(snapshot()[0].Protocol).Should(Equal(model.RequestProtocolTCP))

			cfg.TCPPct = 0
			eng2, snap2 := captureEng(&mockSource{})
			eng2.resolveOne(context.Background(), decoyQuery{name: "udp.example", qtype: dns.TypeA})
			Expect(snap2()[0].Protocol).Should(Equal(model.RequestProtocolUDP))
		})
	})

	Describe("per-client persona attribution (#6)", func() {
		It("stamps a sampled real client's IP and fingerprint", func() {
			src := &mockSource{persona: querylog.ClientPersona{
				IP: "192.168.1.50",
				Fp: querylog.FpSample{
					HadEDNS0:     true,
					EDNSUDPSize:  1232,
					EDNSOptCodes: []uint16{dns.EDNS0COOKIE},
				},
			}}
			cfg.PersonaAttribution = true
			cfg.FingerprintMatch = true
			eng, snapshot := captureEng(src)

			eng.resolveOne(context.Background(), decoyQuery{name: "attr.example", qtype: dns.TypeA})

			req := snapshot()[0]
			Expect(req.ClientIP.String()).Should(Equal("192.168.1.50"))
			Expect(req.Fingerprint.HadEDNS0).Should(BeTrue())
			Expect(req.Req.Extra).ShouldNot(BeEmpty()) // OPT record stamped from the persona
		})
	})

	Describe("adaptive back-off (#7)", func() {
		It("lowers the rate after an error burst and recovers on success", func() {
			eng := NewEngine(cfg, &mockSource{}, nil)
			eng.cfg.AdaptiveBackoff = true

			Expect(eng.backoffFactor()).Should(Equal(1.0))

			// A burst of decoy errors drives the multiplier down.
			for i := 0; i < backoffWindow; i++ {
				eng.noteOutcome(true)
			}
			lowered := eng.backoffFactor()
			Expect(lowered).Should(BeNumerically("<", 1.0))

			// Sustained success recovers it back toward 1.
			for i := 0; i < 200; i++ {
				eng.noteOutcome(false)
			}
			Expect(eng.backoffFactor()).Should(BeNumerically(">", lowered))
			Expect(eng.backoffFactor()).Should(Equal(1.0))
		})

		It("is a no-op when disabled", func() {
			eng := NewEngine(cfg, &mockSource{}, nil)
			eng.cfg.AdaptiveBackoff = false
			for i := 0; i < backoffWindow; i++ {
				eng.noteOutcome(true)
			}
			Expect(eng.backoffFactor()).Should(Equal(1.0))
		})
	})
})
