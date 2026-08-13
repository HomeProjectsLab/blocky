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

type realRow struct {
	RequestTS    time.Time `gorm:"column:request_ts"`
	QuestionName string    `gorm:"column:question_name"`
	QuestionType string    `gorm:"column:question_type"`
	Decoy        bool      `gorm:"column:decoy"`
	EDNSUDPSize  uint16    `gorm:"column:edns_udp_size"`
	EDNSOptCodes string    `gorm:"column:edns_opt_codes"`
	FpDetail     string    `gorm:"column:fp_detail"`
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
				Expect(req.Req.Question[0].Qtype).Should(BeElementOf(dns.TypeA, dns.TypeAAAA))
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
})
