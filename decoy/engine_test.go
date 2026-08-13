//go:build !mips && !mipsle && !mips64 && !mips64le && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package decoy

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
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

type realRow struct {
	RequestTS     time.Time `gorm:"column:request_ts"`
	QuestionName  string    `gorm:"column:question_name"`
	QuestionType  string    `gorm:"column:question_type"`
	Decoy         bool      `gorm:"column:decoy"`
	EDNSUDPSize   uint16    `gorm:"column:edns_udp_size"`
	EDNSOptCodes  string    `gorm:"column:edns_opt_codes"`
	FpDetail      string    `gorm:"column:fp_detail"`
	EffectiveTLDP string    `gorm:"column:effective_tld_p"` // read by SampleFingerprintForName
}

func (realRow) TableName() string { return "log_entries" }

// newSourceDB creates the query-log db with a log_entries table holding the
// given replay rows, then returns a DecoySource over it.
func newSourceDB(replay []realRow) *querylog.DecoySource {
	path := filepath.Join(GinkgoT().TempDir(), "decoy.db")

	raw, e := gorm.Open(sqlite.Open(path), &gorm.Config{})
	Expect(e).Should(Succeed())
	Expect(raw.AutoMigrate(&realRow{})).Should(Succeed())

	if len(replay) > 0 {
		Expect(raw.Create(&replay).Error).Should(Succeed())
	}

	sqlDB, _ := raw.DB()
	Expect(sqlDB.Close()).Should(Succeed())

	src, e := querylog.NewDecoySource(path)
	Expect(e).Should(Succeed())
	DeferCleanup(func() { _ = src.Close() })

	return src
}

var _ = Describe("Engine", func() {
	var cfg config.DecoyConfig

	BeforeEach(func() {
		var e error
		cfg, e = config.WithDefaults[config.DecoyConfig]()
		Expect(e).Should(Succeed())
		cfg.Enable = true
	})

	Describe("Seed (embedded list)", func() {
		It("seeds the embedded placeholder list and samples from it", func() {
			src := newSourceDB(nil)
			eng := NewEngine(cfg, src, nil)

			Expect(eng.Seed()).Should(Succeed())

			d, e := src.SampleList()
			Expect(e).Should(Succeed())
			Expect(d).ShouldNot(BeEmpty())
			Expect(d).Should(ContainSubstring("."))
		})
	})

	Describe("source selection", func() {
		It("empty replay pool falls through to the list (100% list)", func() {
			src := newSourceDB(nil) // no real queries
			_, e := src.SeedIfEmpty(strings.NewReader("list1.com\nlist2.com\n"))
			Expect(e).Should(Succeed())

			cfg.ReplayWeight = 10
			cfg.ListWeight = 1
			eng := NewEngine(cfg, src, nil)

			for i := 0; i < 50; i++ {
				name := eng.nextQuery().name
				Expect(name).Should(BeElementOf("list1.com", "list2.com"))
			}
		})

		It("prefers replay ~10:1 when the pool is populated", func() {
			now := time.Now()
			src := newSourceDB([]realRow{
				{RequestTS: now, QuestionName: "replaytarget.example", QuestionType: "A"},
			})
			_, e := src.SeedIfEmpty(strings.NewReader("liststatic.example\n"))
			Expect(e).Should(Succeed())

			cfg.ReplayWeight = 10
			cfg.CorpusWeight = 0 // isolate replay vs list (corpus is empty here anyway)
			cfg.ListWeight = 1
			eng := NewEngine(cfg, src, nil)

			replay, list := 0, 0
			for i := 0; i < 2000; i++ {
				name := eng.nextQuery().name
				switch name {
				case "replaytarget.example":
					replay++
				case "liststatic.example":
					list++
				}
			}

			Expect(replay + list).Should(Equal(2000))
			// expected ratio 10:1; very loose bounds to avoid flakes
			Expect(replay).Should(BeNumerically(">", list*4))
			Expect(list).Should(BeNumerically(">", 0))
		})

		It("uses only the list when ListWeight dominates", func() {
			now := time.Now()
			src := newSourceDB([]realRow{
				{RequestTS: now, QuestionName: "replaytarget.example", QuestionType: "A"},
			})
			_, e := src.SeedIfEmpty(strings.NewReader("liststatic.example\n"))
			Expect(e).Should(Succeed())

			cfg.ReplayWeight = 0
			cfg.ListWeight = 1
			eng := NewEngine(cfg, src, nil)

			for i := 0; i < 50; i++ {
				name := eng.nextQuery().name
				Expect(name).Should(Equal("liststatic.example"))
			}
		})
	})

	Describe("emit", func() {
		It("marks every emitted request Bypass and Decoy", func() {
			now := time.Now()
			src := newSourceDB([]realRow{
				{RequestTS: now, QuestionName: "replaytarget.example", QuestionType: "AAAA"},
			})
			_, e := src.SeedIfEmpty(strings.NewReader("liststatic.example\n"))
			Expect(e).Should(Succeed())

			cfg.MissChaffPct = 0 // keep this test 1:1 (fan-out toggles have their own tests)
			cfg.ClusterPct = 0
			cfg.CohortPct = 0     // no async recorded-cohort fan-out (has its own test)
			cfg.ChatterPct = 0    // no device-chatter branch (has its own test)
			cfg.FailChaffPct = 0  // no fail-chaff branch (has its own test)
			cfg.DualStackPct = 0  // no A+AAAA pairing fan-out either
			cfg.ShadowTTL = false // same domain repeated 20x would otherwise self-suppress

			var captured []*model.Request
			eng := NewEngine(cfg, src, func(_ context.Context, req *model.Request) (*model.Response, error) {
				captured = append(captured, req)

				return &model.Response{Res: new(dns.Msg)}, nil
			})

			for i := 0; i < 20; i++ {
				eng.emit(context.Background())
			}

			Expect(captured).Should(HaveLen(20))
			for _, req := range captured {
				Expect(req.Bypass).Should(BeTrue())
				Expect(req.Decoy).Should(BeTrue())
				Expect(req.Req.Question).ShouldNot(BeEmpty())
				Expect(req.Req.Question[0].Qtype).Should(
					BeElementOf(dns.TypeA, dns.TypeAAAA, dns.TypeHTTPS, dns.TypeSVCB,
						dns.TypeTXT, dns.TypeMX, dns.TypeNS, dns.TypeSRV))
			}
		})
	})

	Describe("active hours gate", func() {
		It("is within the window for a full-day config", func() {
			cfg.ActiveHoursStart = 0
			cfg.ActiveHoursEnd = 24
			eng := NewEngine(cfg, nil, nil)
			Expect(eng.withinActiveHours(time.Now())).Should(BeTrue())
		})

		It("skips outside the configured window", func() {
			cfg.ActiveHoursStart = 9
			cfg.ActiveHoursEnd = 17
			cfg.ActiveHoursEdgeJitterMin = 0 // deterministic boundaries for this assertion
			eng := NewEngine(cfg, nil, nil)

			at := func(h int) time.Time { return time.Date(2026, 1, 1, h, 0, 0, 0, time.UTC) }
			Expect(eng.withinActiveHours(at(3))).Should(BeFalse())
			Expect(eng.withinActiveHours(at(12))).Should(BeTrue())
			Expect(eng.withinActiveHours(at(17))).Should(BeFalse()) // end exclusive
		})
	})

	Describe("nextInterval", func() {
		It("returns a positive randomized duration", func() {
			eng := NewEngine(cfg, nil, nil)
			Expect(eng.nextInterval()).Should(BeNumerically(">", time.Duration(0)))
		})
	})

	// feedReal marshals a real (or decoy) query-log event as the Hub does and
	// pushes it through the engine's tap, exactly like the live subscription.
	feedReal := func(eng *Engine, ctx context.Context, question string, decoy bool) {
		data, e := json.Marshal(querylog.QueryItem{Question: question, Qtype: "A", Decoy: decoy})
		Expect(e).Should(Succeed())
		eng.tap(ctx, data)
	}

	Describe("rolling real-QPS counter", func() {
		It("counts real events in the window and ignores decoy-flagged ones", func() {
			eng := NewEngine(cfg, newSourceDB(nil), nil)
			eng.cfg.CompanionPct = 0 // isolate the counter from companion side effects

			base := time.Now()
			eng.now = func() time.Time { return base }

			for i := 0; i < 5; i++ {
				feedReal(eng, context.Background(), "real.example", false)
			}
			// decoy-flagged events must not count (feedback guard)
			for i := 0; i < 3; i++ {
				feedReal(eng, context.Background(), "decoy.example", true)
			}

			Expect(eng.recentRealCount()).Should(Equal(5))
		})

		It("ages events out of the window", func() {
			eng := NewEngine(cfg, newSourceDB(nil), nil)
			eng.cfg.CompanionPct = 0

			t := time.Now()
			eng.now = func() time.Time { return t }

			feedReal(eng, context.Background(), "old.example", false)
			Expect(eng.recentRealCount()).Should(Equal(1))

			// jump past the window; the old event must fall out
			t = t.Add(realWindow + time.Second)
			Expect(eng.recentRealCount()).Should(Equal(0))
		})
	})

	Describe("reactive rate", func() {
		It("emits faster under live load than when quiet, and varies", func() {
			eng := NewEngine(cfg, newSourceDB(nil), nil)
			eng.cfg.PersonaCover = false // exercise the reactive fallback path
			eng.cfg.ReactiveVolume = true
			eng.cfg.CompanionPct = 0 // isolate rate from companion side effects
			eng.cfg.DiurnalShaping = false
			eng.cfg.QueriesPerMinute = 4

			base := time.Now()
			eng.now = func() time.Time { return base }

			// quiet: no live events → cold-start fallback (base rate)
			quietQPM := eng.effectiveQPM()
			Expect(quietQPM).Should(Equal(4.0))

			// busy: many real events in the window → much higher rate
			for i := 0; i < 120; i++ {
				feedReal(eng, context.Background(), "real.example", false)
			}

			seen := map[float64]struct{}{}
			for i := 0; i < 30; i++ {
				n, qpm := eng.reactiveQPM()
				Expect(n).Should(Equal(120))
				// 120 real queries in a 60s window ≈ 120 QPM ± jitter, well above quiet
				Expect(qpm).Should(BeNumerically(">", 100))
				seen[qpm] = struct{}{}
			}
			// the ± random jitter term should produce more than one value
			Expect(len(seen)).Should(BeNumerically(">", 1))
		})

		It("falls back to the diurnal/base path at cold start", func() {
			eng := NewEngine(cfg, newSourceDB(nil), nil)
			eng.cfg.PersonaCover = false // exercise the reactive fallback path
			eng.cfg.ReactiveVolume = true
			eng.cfg.QueriesPerMinute = 4
			eng.cfg.DiurnalShaping = false

			// no live events (< minLiveEvents) → effectiveQPM uses the base rate
			Expect(eng.effectiveQPM()).Should(Equal(4.0))
		})
	})

	Describe("browse-triggered companions", func() {
		newCapturingEngine := func() (*Engine, func() []*model.Request) {
			src := newSourceDB(nil)
			_, e := src.SeedIfEmpty(strings.NewReader("poolnoise.example\n"))
			Expect(e).Should(Succeed())

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

		It("derives www.<eTLD+1> and a capped burst from the domain", func() {
			eng, _ := newCapturingEngine()

			for i := 0; i < 20; i++ {
				burst := eng.companionsFor("sub.example.com")
				Expect(len(burst)).Should(BeNumerically(">=", 2))
				Expect(len(burst)).Should(BeNumerically("<=", 4))

				names := make([]string, len(burst))
				for j, q := range burst {
					names[j] = q.name
				}
				Expect(names).Should(ContainElement("www.example.com")) // eTLD+1, not sub.
			}
		})

		It("fires a burst of Bypass+Decoy companions for a real query (pct=100)", func() {
			eng, snapshot := newCapturingEngine()
			eng.cfg.CompanionPct = 100

			feedReal(eng, context.Background(), "example.com", false)

			Eventually(snapshot, "3s", "20ms").Should(Not(BeEmpty()))
			Eventually(func() bool {
				reqs := snapshot()
				for _, r := range reqs {
					q := r.Req.Question[0].Name
					if q == "www.example.com." {
						return true
					}
				}

				return false
			}, "3s", "20ms").Should(BeTrue())

			for _, r := range snapshot() {
				Expect(r.Bypass).Should(BeTrue())
				Expect(r.Decoy).Should(BeTrue())
			}
		})

		It("fires nothing for a decoy-flagged event (feedback guard)", func() {
			eng, snapshot := newCapturingEngine()
			eng.cfg.CompanionPct = 100

			feedReal(eng, context.Background(), "example.com", true) // decoy=true

			Consistently(snapshot, "300ms", "20ms").Should(BeEmpty())
			Expect(eng.recentRealCount()).Should(Equal(0)) // also not counted
		})

		It("fires nothing when CompanionPct is 0", func() {
			eng, snapshot := newCapturingEngine()
			eng.cfg.CompanionPct = 0

			feedReal(eng, context.Background(), "example.com", false)

			Consistently(snapshot, "300ms", "20ms").Should(BeEmpty())
		})

		It("bounds the randomized companion delay", func() {
			eng, _ := newCapturingEngine()

			for i := 0; i < 100; i++ {
				d := eng.companionDelay()
				Expect(d).Should(BeNumerically(">=", companionDelayMinMs*time.Millisecond))
				Expect(d).Should(BeNumerically("<", (companionDelayMinMs+companionDelaySpreadMs)*time.Millisecond))
			}
		})
	})
})
