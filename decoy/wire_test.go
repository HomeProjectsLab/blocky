//go:build !mips && !mipsle && !mips64 && !mips64le && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package decoy

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/resolver"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// wireOptCodes returns the wire-order EDNS0 option codes on a received message.
func wireOptCodes(msg *dns.Msg) []uint16 {
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

// This suite PROVES, at the wire level, that applyFingerprint's OPT actually
// reaches the datagram an upstream unpacks — not just the model.Request struct.
// The engine's resolve func sends req.Req over real UDP to a recording mock; the
// mock unpacks the bytes off the wire and we assert on that unpacked message.
var _ = Describe("Decoy fingerprint on the wire", func() {
	var (
		cfg      config.DecoyConfig
		mu       sync.Mutex
		received []*dns.Msg
	)

	BeforeEach(func() {
		var e error
		cfg, e = config.WithDefaults[config.DecoyConfig]()
		Expect(e).Should(Succeed())
		cfg.Enable = true
		cfg.FingerprintMatch = true
		cfg.MissChaffPct = 0     // keep it a single, un-chaffed decoy
		cfg.ClusterPct = 0       // no fan-out
		cfg.CohortPct = 0        // no async recorded-cohort replay — assert on the single synchronous decoy
		cfg.ReplayMutate = false // don't perturb the replayed query
		cfg.ReplayWeight = 1
		cfg.CorpusWeight = 0
		cfg.ListWeight = 0

		received = nil
	})

	// mockUpstream starts a recording UDP upstream and returns a resolve func that
	// sends req.Req to it over the wire (real pack/unpack), plus the last-received
	// getter.
	mockUpstream := func() (ResolveFunc, func() *dns.Msg) {
		srv := resolver.NewMockUDPUpstreamServer().WithAnswerFn(func(req *dns.Msg) *dns.Msg {
			mu.Lock()
			received = append(received, req.Copy())
			mu.Unlock()

			resp := new(dns.Msg)
			resp.SetReply(req)

			return resp
		})
		up := srv.Start()
		addr := net.JoinHostPort(up.Host, strconv.Itoa(int(up.Port)))
		client := &dns.Client{Net: "udp"}

		resolve := func(_ context.Context, r *model.Request) (*model.Response, error) {
			resp, _, err := client.Exchange(r.Req, addr)

			return &model.Response{Res: resp}, err
		}

		last := func() *dns.Msg {
			mu.Lock()
			defer mu.Unlock()
			Expect(received).NotTo(BeEmpty(), "upstream received no query")

			return received[len(received)-1]
		}

		return resolve, last
	}

	It("reproduces the sampled size/DO/option-codes-in-order + cookie on the datagram", func() {
		// One distinctive real row: cookie(10) + NSID(3), 4096, DO=1, mixed case.
		// 4096 >= the upstream UDP buffer floor, so no normalization masks it.
		src := newSourceDB([]realRow{{
			RequestTS: time.Now(), QuestionName: "target.example", QuestionType: "A",
			EffectiveTLDP: "target.example",
			EDNSUDPSize:   4096, EDNSOptCodes: "10,3",
			FpDetail: `{"qclass":1,"do":true,"hadEdns0":true,"hasCookie":true,"mixed0x20":true}`,
		}})

		resolve, last := mockUpstream()
		eng := NewEngine(cfg, src, resolve)

		eng.emit(context.Background())

		got := last()

		opt := got.IsEdns0()
		Expect(opt).NotTo(BeNil(), "OPT record missing on the wire")
		Expect(opt.UDPSize()).To(Equal(uint16(4096)))
		Expect(opt.Do()).To(BeTrue())
		Expect(wireOptCodes(got)).To(Equal([]uint16{dns.EDNS0COOKIE, dns.EDNS0NSID})) // order preserved

		By("cookie carries a real 8-byte client cookie (16 hex chars)")
		for _, o := range opt.Option {
			if c, ok := o.(*dns.EDNS0_COOKIE); ok {
				Expect(c.Cookie).To(HaveLen(16))
			}
		}

		By("0x20 mixed case reached the qname")
		Expect(got.Question[0].Name).To(MatchRegexp(`[A-Z]`))
	})

	It("cold start (no real rows) sends a plain query, no OPT, no crash", func() {
		src := newSourceDB(nil)
		_, e := src.SeedIfEmpty(strings.NewReader("liststatic.example\n"))
		Expect(e).Should(Succeed())

		resolve, last := mockUpstream()
		eng := NewEngine(cfg, src, resolve)

		eng.emit(context.Background())

		got := last()
		Expect(got.Question).NotTo(BeEmpty())
		Expect(got.IsEdns0()).To(BeNil(), "no real fingerprint to match — must stay plain")
	})
})
