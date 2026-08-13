//go:build !mips && !mipsle && !mips64 && !mips64le && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package querylog

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// realRow mirrors the columns of log_entries that the replay sampler reads, so
// the test can seed the replay pool without depending on the writer internals.
type realRow struct {
	RequestTS     time.Time `gorm:"column:request_ts"`
	QuestionName  string    `gorm:"column:question_name"`
	QuestionType  string    `gorm:"column:question_type"`
	EffectiveTLDP string    `gorm:"column:effective_tld_p"`
	Decoy         bool      `gorm:"column:decoy"`
	EDNSUDPSize   uint16    `gorm:"column:edns_udp_size"`
	EDNSOptCodes  string    `gorm:"column:edns_opt_codes"`
	FpDetail      string    `gorm:"column:fp_detail"`
}

func (realRow) TableName() string { return "log_entries" }

var _ = Describe("DecoySource", func() {
	var (
		path   string
		source *DecoySource
	)

	BeforeEach(func() {
		path = filepath.Join(GinkgoT().TempDir(), "decoy.db")

		// create log_entries so the replay sampler has a table to read
		raw, e := gorm.Open(sqlite.Open(path), &gorm.Config{})
		Expect(e).Should(Succeed())
		Expect(raw.AutoMigrate(&realRow{})).Should(Succeed())

		now := time.Now()
		Expect(raw.Create(&[]realRow{
			{RequestTS: now, QuestionName: "real1.com", QuestionType: "A", Decoy: false},
			{RequestTS: now, QuestionName: "real2.com", QuestionType: "AAAA", Decoy: false},
			{RequestTS: now, QuestionName: "adecoy.com", QuestionType: "A", Decoy: true},
			{RequestTS: now.Add(-30 * 24 * time.Hour), QuestionName: "old.com", QuestionType: "A", Decoy: false},
		}).Error).Should(Succeed())

		sqlDB, _ := raw.DB()
		Expect(sqlDB.Close()).Should(Succeed())

		source, err = NewDecoySource(path)
		Expect(err).Should(Succeed())
		DeferCleanup(func() { _ = source.Close() })
	})

	Describe("SeedIfEmpty + SampleList", func() {
		It("seeds a tiny list and samples domains from it", func() {
			n, e := source.SeedIfEmpty(strings.NewReader("a.com\nb.com\nc.com\n"))
			Expect(e).Should(Succeed())
			Expect(n).Should(Equal(3))

			for i := 0; i < 20; i++ {
				d, e := source.SampleList()
				Expect(e).Should(Succeed())
				Expect(d).Should(BeElementOf("a.com", "b.com", "c.com"))
			}
		})

		It("skips blank lines and does not reseed a populated table", func() {
			n, e := source.SeedIfEmpty(strings.NewReader("a.com\n\n\nb.com\n"))
			Expect(e).Should(Succeed())
			Expect(n).Should(Equal(2))

			n2, e := source.SeedIfEmpty(strings.NewReader("x.com\ny.com\n"))
			Expect(e).Should(Succeed())
			Expect(n2).Should(Equal(0)) // already seeded
		})

		It("returns empty string when the list is not seeded", func() {
			d, e := source.SampleList()
			Expect(e).Should(Succeed())
			Expect(d).Should(BeEmpty())
		})
	})

	Describe("SampleRealFingerprint", func() {
		It("returns the full wire shape parsed from columns + fp_detail", func() {
			// Fresh DB with a single distinctive real row so the random sample is
			// deterministic (only one non-decoy row in the window).
			p := filepath.Join(GinkgoT().TempDir(), "fp.db")
			raw, e := gorm.Open(sqlite.Open(p), &gorm.Config{})
			Expect(e).Should(Succeed())
			Expect(raw.AutoMigrate(&realRow{})).Should(Succeed())
			Expect(raw.Create(&realRow{
				RequestTS: time.Now(), QuestionName: "fp.example", QuestionType: "AAAA",
				EDNSUDPSize: 4096, EDNSOptCodes: "10,3",
				FpDetail: `{"qclass":1,"do":true,"hadEdns0":true,"hasCookie":true,"mixed0x20":true}`,
			}).Error).Should(Succeed())
			sqlDB, _ := raw.DB()
			Expect(sqlDB.Close()).Should(Succeed())

			src, e := NewDecoySource(p)
			Expect(e).Should(Succeed())
			DeferCleanup(func() { _ = src.Close() })

			fp, e := src.SampleRealFingerprint()
			Expect(e).Should(Succeed())
			Expect(fp.Qtype).Should(Equal("AAAA"))
			Expect(fp.EDNSUDPSize).Should(Equal(uint16(4096)))
			Expect(fp.EDNSOptCodes).Should(Equal([]uint16{10, 3})) // wire order preserved
			Expect(fp.HadEDNS0).Should(BeTrue())
			Expect(fp.DO).Should(BeTrue())
			Expect(fp.HasCookie).Should(BeTrue())
			Expect(fp.Mixed0x20).Should(BeTrue())
			Expect(fp.QClass).Should(Equal(uint16(1)))
		})

		It("cold start (no real rows) yields the zero value, no error", func() {
			p := filepath.Join(GinkgoT().TempDir(), "empty.db")
			raw, e := gorm.Open(sqlite.Open(p), &gorm.Config{})
			Expect(e).Should(Succeed())
			Expect(raw.AutoMigrate(&realRow{})).Should(Succeed())
			sqlDB, _ := raw.DB()
			Expect(sqlDB.Close()).Should(Succeed())

			src, e := NewDecoySource(p)
			Expect(e).Should(Succeed())
			DeferCleanup(func() { _ = src.Close() })

			fp, e := src.SampleRealFingerprint()
			Expect(e).Should(Succeed())
			Expect(fp.HadEDNS0).Should(BeFalse())
			Expect(fp.EDNSOptCodes).Should(BeEmpty())
		})
	})

	Describe("SampleRecentReal", func() {
		It("returns only recent non-decoy queries", func() {
			rows, e := source.SampleRecentReal(10)
			Expect(e).Should(Succeed())
			Expect(rows).Should(HaveLen(2)) // real1, real2; decoy + old excluded

			names := []string{rows[0].Name, rows[1].Name}
			Expect(names).Should(ConsistOf("real1.com", "real2.com"))
		})
	})

	Describe("noise corpus (T3)", func() {
		It("upserts real non-blocked queries and samples them back", func() {
			now := time.Now()
			Expect(upsertNoiseCorpus(source.db, []*logEntry{
				{QuestionName: "corpus1.com", RequestTS: now},
				{QuestionName: "corpus1.com", RequestTS: now}, // second hit bumps count
				{QuestionName: "corpus2.com", RequestTS: now},
				{QuestionName: "blocked.com", RequestTS: now, ResponseType: "BLOCKED"}, // skipped
				{QuestionName: "adecoy.com", RequestTS: now, Decoy: true},              // skipped
				{QuestionName: "", RequestTS: now},                                     // skipped
			})).Should(Succeed())

			var hits int64
			Expect(source.db.Raw("SELECT hits FROM noise_corpus WHERE domain = ?", "corpus1.com").
				Scan(&hits).Error).Should(Succeed())
			Expect(hits).Should(Equal(int64(2)))

			var n int64
			Expect(source.db.Raw("SELECT COUNT(*) FROM noise_corpus").Scan(&n).Error).Should(Succeed())
			Expect(n).Should(Equal(int64(2))) // only the two real, non-blocked domains

			for i := 0; i < 20; i++ {
				d, e := source.SampleCorpus()
				Expect(e).Should(Succeed())
				Expect(d).Should(BeElementOf("corpus1.com", "corpus2.com"))
			}
		})

		It("returns empty string on an empty corpus", func() {
			d, e := source.SampleCorpus()
			Expect(e).Should(Succeed())
			Expect(d).Should(BeEmpty())
		})

		It("LRU-prunes down to the cap by last_seen", func() {
			orig := noiseCorpusCap
			noiseCorpusCap = 5
			DeferCleanup(func() { noiseCorpusCap = orig })

			base := time.Now().Add(-time.Hour)

			entries := make([]*logEntry, 0, 8)
			for i := 0; i < 8; i++ {
				entries = append(entries, &logEntry{
					QuestionName: "c" + strconv.Itoa(i) + ".com",
					RequestTS:    base.Add(time.Duration(i) * time.Minute), // c0 oldest, c7 newest
				})
			}
			Expect(upsertNoiseCorpus(source.db, entries)).Should(Succeed())

			Expect(pruneNoiseCorpus(source.db)).Should(Succeed())

			var n int64
			Expect(source.db.Raw("SELECT COUNT(*) FROM noise_corpus").Scan(&n).Error).Should(Succeed())
			Expect(n).Should(Equal(int64(5)))

			// the 3 oldest (c0,c1,c2) are gone; the 5 newest survive
			var survivors []string
			Expect(source.db.Raw("SELECT domain FROM noise_corpus ORDER BY domain").Scan(&survivors).Error).Should(Succeed())
			Expect(survivors).Should(ConsistOf("c3.com", "c4.com", "c5.com", "c6.com", "c7.com"))
		})
	})

	Describe("IsBlockedDomain + block exclusion (T6)", func() {
		BeforeEach(func() {
			_, e := source.SeedBlocklistIfEmpty("ads", strings.NewReader("bad.com\n"))
			Expect(e).Should(Succeed())
		})

		It("is true for a seeded blocklist domain, false otherwise", func() {
			b, e := source.IsBlockedDomain("bad.com")
			Expect(e).Should(Succeed())
			Expect(b).Should(BeTrue())

			b, e = source.IsBlockedDomain("good.com")
			Expect(e).Should(Succeed())
			Expect(b).Should(BeFalse())
		})

		It("SampleList never returns a blocked domain", func() {
			_, e := source.SeedIfEmpty(strings.NewReader("bad.com\ngood1.com\ngood2.com\ngood3.com\n"))
			Expect(e).Should(Succeed())

			for i := 0; i < 100; i++ {
				d, e := source.SampleList()
				Expect(e).Should(Succeed())
				Expect(d).ShouldNot(Equal("bad.com"))
			}
		})
	})

	Describe("SampleList rank-weighting (T7)", func() {
		It("skews draws toward low rowids (popular ranks)", func() {
			p := filepath.Join(GinkgoT().TempDir(), "rank.db")
			raw, e := gorm.Open(sqlite.Open(p), &gorm.Config{})
			Expect(e).Should(Succeed())
			Expect(raw.AutoMigrate(&realRow{})).Should(Succeed())
			sqlDB, _ := raw.DB()
			Expect(sqlDB.Close()).Should(Succeed())

			src, e := NewDecoySource(p)
			Expect(e).Should(Succeed())
			DeferCleanup(func() { _ = src.Close() })

			const total = 1000

			var b strings.Builder
			for i := 1; i <= total; i++ {
				b.WriteString("r" + strconv.Itoa(i) + ".com\n") // rowid i == rank i
			}
			_, e = src.SeedIfEmpty(strings.NewReader(b.String()))
			Expect(e).Should(Succeed())

			topDecile := 0
			const draws = 2000
			for i := 0; i < draws; i++ {
				d, e := src.SampleList()
				Expect(e).Should(Succeed())
				rank, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(d, "r"), ".com"))
				if rank <= total/10 {
					topDecile++
				}
			}

			// Zipf s≈1 puts ~ln(100)/ln(1000) ≈ 0.67 of draws in the top decile;
			// uniform would give 0.10. Loose lower bound well clear of uniform.
			Expect(float64(topDecile) / float64(draws)).Should(BeNumerically(">", 0.4))
		})
	})

	Describe("SampleFingerprintForName (T13)", func() {
		It("returns the name's own eTLD+1 fingerprint when present, else falls back", func() {
			p := filepath.Join(GinkgoT().TempDir(), "fpname.db")
			raw, e := gorm.Open(sqlite.Open(p), &gorm.Config{})
			Expect(e).Should(Succeed())
			Expect(raw.AutoMigrate(&realRow{})).Should(Succeed())
			Expect(raw.Create(&[]realRow{
				{
					RequestTS: time.Now(), QuestionName: "api.example.com", QuestionType: "AAAA",
					EffectiveTLDP: "example.com", EDNSUDPSize: 1232, EDNSOptCodes: "10,8",
					FpDetail: `{"hadEdns0":true}`,
				},
				{
					RequestTS: time.Now(), QuestionName: "other.net", QuestionType: "A",
					EffectiveTLDP: "other.net", EDNSUDPSize: 512,
				},
			}).Error).Should(Succeed())
			sqlDB, _ := raw.DB()
			Expect(sqlDB.Close()).Should(Succeed())

			src, e := NewDecoySource(p)
			Expect(e).Should(Succeed())
			DeferCleanup(func() { _ = src.Close() })

			// a subdomain of a known eTLD+1 gets that eTLD+1's stable shape
			fp, e := src.SampleFingerprintForName("cdn.example.com")
			Expect(e).Should(Succeed())
			Expect(fp.EDNSUDPSize).Should(Equal(uint16(1232)))
			Expect(fp.EDNSOptCodes).Should(Equal([]uint16{10, 8}))
			Expect(fp.HadEDNS0).Should(BeTrue())

			// an unseen eTLD+1 falls back to a random real fp (one of the two rows)
			fp, e = src.SampleFingerprintForName("nothing.here.org")
			Expect(e).Should(Succeed())
			Expect(fp.EDNSUDPSize).Should(BeElementOf(uint16(1232), uint16(512)))
		})
	})
})
