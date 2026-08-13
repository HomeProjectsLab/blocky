//go:build !mips && !mipsle && !mips64 && !mips64le && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package querylog

import (
	"path/filepath"
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
	RequestTS    time.Time `gorm:"column:request_ts"`
	QuestionName string    `gorm:"column:question_name"`
	QuestionType string    `gorm:"column:question_type"`
	Decoy        bool      `gorm:"column:decoy"`
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

	Describe("SampleRecentReal", func() {
		It("returns only recent non-decoy queries", func() {
			rows, e := source.SampleRecentReal(10)
			Expect(e).Should(Succeed())
			Expect(rows).Should(HaveLen(2)) // real1, real2; decoy + old excluded

			names := []string{rows[0].Name, rows[1].Name}
			Expect(names).Should(ConsistOf("real1.com", "real2.com"))
		})
	})
})
