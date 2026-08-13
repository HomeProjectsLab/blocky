//go:build !mips && !mipsle && !mips64 && !mips64le && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package decoy

import (
	"context"
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

// fpRow mirrors the log_entries columns SampleRealFingerprint reads.
type fpRow struct {
	RequestTS     time.Time `gorm:"column:request_ts"`
	QuestionName  string    `gorm:"column:question_name"`
	QuestionType  string    `gorm:"column:question_type"`
	Decoy         bool      `gorm:"column:decoy"`
	EDNSUDPSize   uint16    `gorm:"column:edns_udp_size"`
	EDNSOptCodes  string    `gorm:"column:edns_opt_codes"`
	FpDetail      string    `gorm:"column:fp_detail"`
	EffectiveTLDP string    `gorm:"column:effective_tldp"` // read by SampleFingerprintForName
}

func (fpRow) TableName() string { return "log_entries" }

func newSourceWithFp(rows []fpRow) *querylog.DecoySource {
	path := filepath.Join(GinkgoT().TempDir(), "decoy.db")

	raw, e := gorm.Open(sqlite.Open(path), &gorm.Config{})
	Expect(e).Should(Succeed())
	Expect(raw.AutoMigrate(&fpRow{})).Should(Succeed())

	if len(rows) > 0 {
		Expect(raw.Create(&rows).Error).Should(Succeed())
	}

	sqlDB, _ := raw.DB()
	Expect(sqlDB.Close()).Should(Succeed())

	src, e := querylog.NewDecoySource(path)
	Expect(e).Should(Succeed())
	DeferCleanup(func() { _ = src.Close() })

	return src
}

var _ = Describe("Obfuscation techniques", func() {
	var cfg config.DecoyConfig

	BeforeEach(func() {
		var e error
		cfg, e = config.WithDefaults[config.DecoyConfig]()
		Expect(e).Should(Succeed())
		cfg.Enable = true
	})

	Describe("technique 1: diurnal shape factor", func() {
		It("returns 1 at cold start (no history)", func() {
			var zero [24]int64
			Expect(diurnalShape(zero, 10)).Should(Equal(1.0))
		})

		It("returns 1 when every hour is equal (share == average)", func() {
			var flat [24]int64
			for i := range flat {
				flat[i] = 100
			}
			Expect(diurnalShape(flat, 7)).Should(BeNumerically("~", 1.0, 1e-9))
		})

		It("scales above 1 for a busy hour, clamped at 3", func() {
			var c [24]int64
			c[10] = 1000 // one dominant hour → raw factor 24, clamped
			Expect(diurnalShape(c, 10)).Should(Equal(3.0))
		})

		It("scales below 1 for a quiet hour, clamped at 0.2", func() {
			var c [24]int64
			for i := range c {
				c[i] = 1000
			}
			c[3] = 1 // near-empty hour
			Expect(diurnalShape(c, 3)).Should(Equal(0.2))
		})

		It("gives a proportional factor in-band", func() {
			var c [24]int64
			for i := range c {
				c[i] = 10
			}
			c[5] = 20 // hour5 twice the average
			Expect(diurnalShape(c, 5)).Should(BeNumerically("~", 20.0/(250.0/24.0), 1e-9))
		})
	})

	Describe("technique 2: replay mutation", func() {
		It("produces non-identical variants (qtype flip and subdomain seen)", func() {
			eng := NewEngine(cfg, nil, nil)
			base := decoyQuery{name: "replaytarget.example", qtype: dns.TypeA, replay: true}

			sawQtypeFlip, sawNameChange := false, false
			for i := 0; i < 500; i++ {
				m := eng.mutate(base)
				if m.qtype != base.qtype {
					sawQtypeFlip = true
				}
				if m.name != base.name {
					sawNameChange = true
				}
			}

			Expect(sawQtypeFlip).Should(BeTrue())
			Expect(sawNameChange).Should(BeTrue())
		})
	})

	Describe("technique 3: fingerprint match", func() {
		It("stamps EDNS (UDP size + DO) sampled from a real client", func() {
			src := newSourceWithFp([]fpRow{{
				RequestTS: time.Now(), QuestionName: "real.example", QuestionType: "A",
				EDNSUDPSize: 1232, FpDetail: `{"do":true,"hadEdns0":true}`,
			}})
			eng := NewEngine(cfg, src, nil)

			req := &model.Request{Req: new(dns.Msg)}
			req.Req.SetQuestion("decoy.example.", dns.TypeA)
			eng.applyFingerprint(req)

			opt := req.Req.IsEdns0()
			Expect(opt).ShouldNot(BeNil())
			Expect(opt.UDPSize()).Should(Equal(uint16(1232)))
			Expect(opt.Do()).Should(BeTrue())
			Expect(req.Fingerprint.HadEDNS0).Should(BeTrue())
		})

		It("leaves the query plain at cold start (no real rows)", func() {
			src := newSourceWithFp(nil)
			eng := NewEngine(cfg, src, nil)

			req := &model.Request{Req: new(dns.Msg)}
			req.Req.SetQuestion("decoy.example.", dns.TypeA)
			eng.applyFingerprint(req)

			Expect(req.Req.IsEdns0()).Should(BeNil())
			Expect(req.Fingerprint.HadEDNS0).Should(BeFalse())
		})
	})

	Describe("technique 5: NXDOMAIN/miss chaff", func() {
		It("emits a random label under a real domain", func() {
			src := newSourceWithFp(nil)
			_, e := src.SeedIfEmpty(strings.NewReader("example.com\n"))
			Expect(e).Should(Succeed())

			cfg.MissChaffPct = 100
			cfg.ClusterPct = 0

			var captured []*model.Request
			eng := NewEngine(cfg, src, func(_ context.Context, req *model.Request) (*model.Response, error) {
				captured = append(captured, req)

				return &model.Response{Res: new(dns.Msg)}, nil
			})

			eng.emit(context.Background())

			Expect(captured).Should(HaveLen(1))
			name := captured[0].Req.Question[0].Name
			Expect(name).Should(HaveSuffix("example.com."))
			Expect(name).ShouldNot(Equal("example.com."))                   // extra label prepended
			Expect(strings.Count(name, ".")).Should(BeNumerically(">=", 2)) // multi-label
		})
	})

	Describe("technique 6: sibling-cluster decoys", func() {
		It("emits a capped 2-4 burst, all Bypass+Decoy", func() {
			src := newSourceWithFp(nil)
			_, e := src.SeedIfEmpty(strings.NewReader("example.com\n"))
			Expect(e).Should(Succeed())

			cfg.MissChaffPct = 0
			cfg.ChatterPct = 0   // no lone device-chatter emission
			cfg.FailChaffPct = 0 // no lone fail-chaff emission
			cfg.ClusterPct = 100

			for trial := 0; trial < 25; trial++ {
				var captured []*model.Request
				eng := NewEngine(cfg, src, func(_ context.Context, req *model.Request) (*model.Response, error) {
					captured = append(captured, req)

					return &model.Response{Res: new(dns.Msg)}, nil
				})

				eng.emit(context.Background())

				Expect(len(captured)).Should(BeNumerically(">=", 2))
				Expect(len(captured)).Should(BeNumerically("<=", 4))
				for _, req := range captured {
					Expect(req.Bypass).Should(BeTrue())
					Expect(req.Decoy).Should(BeTrue())
				}
			}
		})
	})

	Describe("technique 4: split-upstream client identity", func() {
		It("varies the client IP when enabled, fixed loopback when disabled", func() {
			eng := NewEngine(cfg, nil, nil)

			cfg.SplitUpstream = false
			eng.cfg = cfg
			Expect(eng.clientIP().String()).Should(Equal(decoyClientIP))

			cfg.SplitUpstream = true
			eng.cfg = cfg
			seen := map[string]bool{}
			for i := 0; i < 200; i++ {
				seen[eng.clientIP().String()] = true
			}
			Expect(len(seen)).Should(BeNumerically(">", 1))
		})
	})
})
