//go:build !mips && !mipsle && !mips64 && !mips64le && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package decoy

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/miekg/dns"
	"gorm.io/gorm"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/querylog"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// corpusSeed / blockSeed map the persistent-corpus and blocklist tables so tests
// can pre-seed them before opening the DecoySource (AutoMigrate is idempotent).
type corpusSeed struct {
	Domain    string    `gorm:"column:domain;primaryKey"`
	FirstSeen time.Time `gorm:"column:first_seen"`
	LastSeen  time.Time `gorm:"column:last_seen"`
	Hits      int64     `gorm:"column:hits"`
}

func (corpusSeed) TableName() string { return "noise_corpus" }

type blockSeed struct {
	Category string `gorm:"column:category;primaryKey"`
	Domain   string `gorm:"column:domain;primaryKey"`
}

func (blockSeed) TableName() string { return "blocklist_domains" }

// newHardeningSource builds a query-log db pre-seeded with replay rows, corpus
// rows and blocklist rows, then returns a DecoySource over it.
func newHardeningSource(replay []realRow, corpus []corpusSeed, blocked []blockSeed) *querylog.DecoySource {
	path := filepath.Join(GinkgoT().TempDir(), "decoy.db")

	raw, e := gorm.Open(sqlite.Open(path), &gorm.Config{})
	Expect(e).Should(Succeed())
	Expect(raw.AutoMigrate(&realRow{}, &corpusSeed{}, &blockSeed{})).Should(Succeed())

	if len(replay) > 0 {
		Expect(raw.Create(&replay).Error).Should(Succeed())
	}

	if len(corpus) > 0 {
		Expect(raw.Create(&corpus).Error).Should(Succeed())
	}

	if len(blocked) > 0 {
		Expect(raw.Create(&blocked).Error).Should(Succeed())
	}

	sqlDB, _ := raw.DB()
	Expect(sqlDB.Close()).Should(Succeed())

	src, e := querylog.NewDecoySource(path)
	Expect(e).Should(Succeed())
	DeferCleanup(func() { _ = src.Close() })

	return src
}

var _ = Describe("Egress hardening", func() {
	var cfg config.DecoyConfig

	BeforeEach(func() {
		var e error
		cfg, e = config.WithDefaults[config.DecoyConfig]()
		Expect(e).Should(Succeed())
		cfg.Enable = true
	})

	Describe("T3: persistent corpus in the source mix", func() {
		It("draws from the corpus when weighted to it", func() {
			now := time.Now()
			src := newHardeningSource(nil,
				[]corpusSeed{{Domain: "mycorpus.example", FirstSeen: now, LastSeen: now, Hits: 3}}, nil)
			_, e := src.SeedIfEmpty(strings.NewReader("publiclist.example\n"))
			Expect(e).Should(Succeed())

			cfg.ReplayWeight = 0
			cfg.CorpusWeight = 1
			cfg.ListWeight = 0
			eng := NewEngine(cfg, src, nil)

			for range 50 {
				Expect(eng.nextQuery().name).Should(Equal("mycorpus.example"))
			}
		})
	})

	Describe("T6: blocked domains never egress", func() {
		It("skips a blocked decoy at the resolve chokepoint and passes an unblocked one", func() {
			src := newHardeningSource(nil, nil, []blockSeed{{Category: "ads", Domain: "blocked.example"}})

			var captured []string
			eng := NewEngine(cfg, src, func(_ context.Context, req *model.Request) (*model.Response, error) {
				captured = append(captured, req.Req.Question[0].Name)

				return &model.Response{Res: new(dns.Msg)}, nil
			})

			eng.resolveOne(context.Background(), decoyQuery{name: "blocked.example", qtype: dns.TypeA})
			eng.resolveOne(context.Background(), decoyQuery{name: "allowed.example", qtype: dns.TypeA})

			Expect(captured).Should(ConsistOf("allowed.example."))
		})
	})

	Describe("T5: shadow-TTL suppression", func() {
		It("suppresses re-emit within the observed TTL and allows it after", func() {
			src := newHardeningSource(nil, nil, nil)

			var calls int
			eng := NewEngine(cfg, src, func(_ context.Context, req *model.Request) (*model.Response, error) {
				calls++

				a := &dns.A{
					Hdr: dns.RR_Header{Name: req.Req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 100},
					A:   net.ParseIP("192.0.2.1"),
				}
				msg := new(dns.Msg)
				msg.Answer = []dns.RR{a}

				return &model.Response{Res: msg}, nil
			})
			eng.cfg.FingerprintMatch = false // isolate suppression from fp side effects

			base := time.Now()
			eng.now = func() time.Time { return base }

			q := decoyQuery{name: "ttl.example", qtype: dns.TypeA}

			eng.resolveOne(context.Background(), q)
			Expect(calls).Should(Equal(1))

			// within TTL → suppressed
			base = base.Add(50 * time.Second)
			eng.resolveOne(context.Background(), q)
			Expect(calls).Should(Equal(1))

			// past TTL → allowed again
			base = base.Add(60 * time.Second)
			eng.resolveOne(context.Background(), q)
			Expect(calls).Should(Equal(2))
		})

		It("does not suppress when ShadowTTL is off", func() {
			src := newHardeningSource(nil, nil, nil)

			var calls int
			eng := NewEngine(cfg, src, func(_ context.Context, _ *model.Request) (*model.Response, error) {
				calls++

				return &model.Response{Res: new(dns.Msg)}, nil
			})
			eng.cfg.ShadowTTL = false
			eng.cfg.FingerprintMatch = false

			q := decoyQuery{name: "nottl.example", qtype: dns.TypeA}
			eng.resolveOne(context.Background(), q)
			eng.resolveOne(context.Background(), q)
			Expect(calls).Should(Equal(2))
		})
	})

	Describe("T10: never gate to zero + edge jitter", func() {
		It("emits at the low floor outside active hours, base rate inside", func() {
			cfg.ActiveHoursStart = 9
			cfg.ActiveHoursEnd = 17
			cfg.ActiveHoursEdgeJitterMin = 0
			cfg.OffHoursFloorQPM = 0.5
			cfg.QueriesPerMinute = 4
			cfg.DiurnalShaping = false
			cfg.ReactiveVolume = false
			cfg.PersonaCover = false // this test exercises the base-rate path, not the persona curve
			eng := NewEngine(cfg, nil, nil)

			at := func(h int) time.Time { return time.Date(2026, 1, 1, h, 0, 0, 0, time.UTC) }

			eng.now = func() time.Time { return at(3) } // outside window
			Expect(eng.effectiveQPM()).Should(Equal(0.5))
			Expect(eng.effectiveQPM()).Should(BeNumerically(">", 0)) // never zero

			eng.now = func() time.Time { return at(12) } // inside window
			Expect(eng.effectiveQPM()).Should(Equal(4.0))
		})

		It("jitters the window edges by <= the configured minutes, varying by day", func() {
			cfg.ActiveHoursStart = 9
			cfg.ActiveHoursEnd = 17
			cfg.ActiveHoursEdgeJitterMin = 30
			eng := NewEngine(cfg, nil, nil)

			seen := map[int]struct{}{}
			for d := 1; d <= 60; d++ {
				start, end := eng.activeEdges(time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC))
				Expect(start).Should(BeNumerically(">=", 9*60-30))
				Expect(start).Should(BeNumerically("<=", 9*60+30))
				Expect(end).Should(BeNumerically(">=", 17*60-30))
				Expect(end).Should(BeNumerically("<=", 17*60+30))
				seen[start] = struct{}{}
			}
			Expect(len(seen)).Should(BeNumerically(">", 1)) // edges move across days
		})
	})

	Describe("T12: qtype realism", func() {
		It("never emits a CHAOS class from mutate", func() {
			eng := NewEngine(cfg, nil, nil)
			base := decoyQuery{name: "replay.example", qtype: dns.TypeA, replay: true}

			for range 2000 {
				Expect(eng.mutate(base).qclass).ShouldNot(Equal(uint16(dns.ClassCHAOS)))
			}
		})

		It("mixes in non-A/AAAA qtypes (HTTPS/SVCB)", func() {
			eng := NewEngine(cfg, nil, nil)

			seen := map[uint16]struct{}{}
			for range 2000 {
				seen[eng.realQtype()] = struct{}{}
			}
			Expect(seen).Should(HaveKey(dns.TypeA))
			Expect(seen).Should(HaveKey(dns.TypeAAAA))
			// at least one non-address qtype must appear
			_, https := seen[dns.TypeHTTPS]
			_, svcb := seen[dns.TypeSVCB]
			Expect(https || svcb).Should(BeTrue())
		})
	})

	Describe("T9: miss-chaff parent + label", func() {
		It("draws the parent from the public list, never the replayed real domain, with a word-like label", func() {
			now := time.Now()
			src := newHardeningSource([]realRow{
				{RequestTS: now, QuestionName: "realsecret.example", QuestionType: "A"},
			}, nil, nil)
			_, e := src.SeedIfEmpty(strings.NewReader("publicparent.example\n"))
			Expect(e).Should(Succeed())

			cfg.MissChaffPct = 100
			cfg.ClusterPct = 0
			cfg.ReplayWeight = 100 // force nextQuery toward the real domain...
			cfg.CorpusWeight = 0
			cfg.ListWeight = 0
			cfg.FingerprintMatch = false

			var names []string
			eng := NewEngine(cfg, src, func(_ context.Context, req *model.Request) (*model.Response, error) {
				names = append(names, req.Req.Question[0].Name)

				return &model.Response{Res: new(dns.Msg)}, nil
			})

			for range 40 {
				eng.emit(context.Background())
			}

			Expect(names).ShouldNot(BeEmpty())
			for _, n := range names {
				Expect(n).Should(HaveSuffix("publicparent.example.")) // ...but the miss parent is the LIST domain
				Expect(n).ShouldNot(ContainSubstring("realsecret"))

				label := strings.Split(n, ".")[0]
				Expect(label).Should(MatchRegexp(`^[a-z]+[0-9]?$`)) // word-like, not flat [a-z0-9]
				Expect(len(label)).Should(BeNumerically(">=", 4))
			}
		})
	})

	Describe("T8: companion template randomization", func() {
		It("varies membership and order across bursts, always keeping www.<eTLD+1>", func() {
			src := newHardeningSource(nil, nil, nil)
			_, e := src.SeedIfEmpty(strings.NewReader("noise1.example\nnoise2.example\n"))
			Expect(e).Should(Succeed())
			eng := NewEngine(cfg, src, nil)

			signatures := map[string]struct{}{}
			for range 60 {
				burst := eng.companionsFor("example.com")
				Expect(len(burst)).Should(BeNumerically(">=", 2))
				Expect(len(burst)).Should(BeNumerically("<=", 4))

				names := make([]string, len(burst))
				for j, q := range burst {
					names[j] = q.name
				}
				Expect(names).Should(ContainElement("www.example.com"))
				signatures[strings.Join(names, ",")] = struct{}{}
			}
			// a fixed template would yield one signature; randomization yields many
			Expect(len(signatures)).Should(BeNumerically(">", 3))
		})
	})

	Describe("T13: fingerprint + cookie stability", func() {
		It("keeps the OPT shape and cookie stable across a decoy name's appearances", func() {
			now := time.Now()
			src := newHardeningSource([]realRow{{
				RequestTS: now, QuestionName: "stable.example", QuestionType: "A",
				EffectiveTLDP: "stable.example", EDNSUDPSize: 1232, EDNSOptCodes: "10",
				FpDetail: `{"hadEdns0":true,"do":true,"hasCookie":true}`,
			}}, nil, nil)

			cfg.SplitUpstream = false // one synthetic client → one cookie
			eng := NewEngine(cfg, src, nil)

			shape := func() (uint16, string) {
				req := &model.Request{ClientIP: net.ParseIP(decoyClientIP), Req: new(dns.Msg)}
				req.Req.SetQuestion("stable.example.", dns.TypeA)
				eng.applyFingerprint(req)

				opt := req.Req.IsEdns0()
				Expect(opt).ShouldNot(BeNil())

				var cookie string
				for _, o := range opt.Option {
					if c, ok := o.(*dns.EDNS0_COOKIE); ok {
						cookie = c.Cookie
					}
				}

				return opt.UDPSize(), cookie
			}

			size1, cookie1 := shape()
			size2, cookie2 := shape()

			Expect(size1).Should(Equal(size2))
			Expect(cookie1).ShouldNot(BeEmpty())
			Expect(cookie1).Should(Equal(cookie2)) // stable across appearances
		})

		It("mints one stable cookie per synthetic client", func() {
			eng := NewEngine(cfg, nil, nil)

			a1 := eng.cookieFor("127.0.0.1")
			a2 := eng.cookieFor("127.0.0.1")
			b := eng.cookieFor("10.0.0.254")

			Expect(a1).Should(HaveLen(16)) // 8 bytes hex
			Expect(a1).Should(Equal(a2))   // same client → same cookie
			Expect(a1).ShouldNot(Equal(b)) // different client → different cookie
		})
	})
})
