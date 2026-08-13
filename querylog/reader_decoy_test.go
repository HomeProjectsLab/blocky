//go:build !mips && !mipsle && !mips64 && !mips64le && !loong64 && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package querylog

import (
	"context"
	"path/filepath"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Reader decoy stats", func() {
	var (
		reader *Reader
		hour1  time.Time
		hour2  time.Time
	)

	decoyEntry := func(start time.Time, question, source string) *LogEntry {
		return &LogEntry{
			Start:        start,
			ClientIP:     "127.0.0.1",
			ClientNames:  []string{"decoy"},
			QuestionName: question,
			QuestionType: "A",
			ResponseType: "RESOLVED",
			ResponseCode: "NOERROR",
			DurationMs:   5,
			Decoy:        true,
			DecoySource:  source,
			Fingerprint:  model.Fingerprint{Transport: model.TransportDo53UDP},
		}
	}

	realEntry := func(start time.Time, question string) *LogEntry {
		return &LogEntry{
			Start:        start,
			ClientIP:     "192.168.1.10",
			ClientNames:  []string{"laptop"},
			QuestionName: question,
			QuestionType: "A",
			ResponseType: "RESOLVED",
			ResponseCode: "NOERROR",
			DurationMs:   30,
			Fingerprint:  model.Fingerprint{Transport: model.TransportDo53UDP},
		}
	}

	BeforeEach(func() {
		ctx, cancelFn := context.WithCancel(context.Background())
		DeferCleanup(cancelFn)

		dbPath := filepath.Join(GinkgoT().TempDir(), "querylog.db")

		writer, err := NewDatabaseWriter(ctx, config.QueryLogTypeSqlite, dbPath, 7, time.Minute)
		Expect(err).Should(Succeed())

		sqlDB, err := writer.db.DB()
		Expect(err).Should(Succeed())
		DeferCleanup(func() error {
			cancelFn()

			return sqlDB.Close()
		})

		hour1 = time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
		hour2 = hour1.Add(time.Hour)

		// hour1: 3 replay (alpha.com), 1 list (gamma.net); hour2: 2 corpus (beta.com)
		for i := range 3 {
			writer.Write(decoyEntry(hour1.Add(time.Duration(i)*time.Minute), "www.alpha.com.", "replay"))
		}
		writer.Write(decoyEntry(hour1.Add(5*time.Minute), "gamma.net.", "list"))
		writer.Write(decoyEntry(hour2.Add(1*time.Minute), "www.beta.com.", "corpus"))
		writer.Write(decoyEntry(hour2.Add(2*time.Minute), "img.beta.com.", "corpus"))

		// real rows: must be excluded from every decoy-scoped stat
		writer.Write(realEntry(hour1.Add(1*time.Minute), "www.real.org."))
		writer.Write(realEntry(hour1.Add(2*time.Minute), "www.real.org."))

		Expect(writer.Flush()).Should(Succeed())

		reader, err = NewReader(dbPath)
		Expect(err).Should(Succeed())
		DeferCleanup(reader.Close)
	})

	from := func() time.Time { return hour1.Add(-time.Hour) }
	to := func() time.Time { return hour2.Add(time.Hour) }

	It("persists decoy_source on the raw row", func() {
		_, items, err := reader.Search(SearchFilter{From: from(), To: to(), IncludeDecoys: true, Domain: "alpha"})
		Expect(err).Should(Succeed())
		Expect(items).ShouldNot(BeEmpty())
		Expect(items[0].DecoySource).Should(Equal("replay"))
	})

	Describe("DecoyOverview", func() {
		It("totals decoys, distinct fake domains and the source breakdown, excluding real rows", func() {
			o, err := reader.DecoyOverview(from(), to())
			Expect(err).Should(Succeed())

			Expect(o.Decoys).Should(BeNumerically("==", 6)) // 6 decoys, 2 real excluded
			Expect(o.DistinctDomains).Should(BeNumerically("==", 3))
			Expect(o.BySource).Should(HaveKeyWithValue("replay", int64(3)))
			Expect(o.BySource).Should(HaveKeyWithValue("corpus", int64(2)))
			Expect(o.BySource).Should(HaveKeyWithValue("list", int64(1)))
			Expect(o.BySource).ShouldNot(HaveKey("")) // no real-row bucket
		})
	})

	Describe("DecoySourceMix", func() {
		It("counts per source, most frequent first", func() {
			mix, err := reader.DecoySourceMix(from(), to())
			Expect(err).Should(Succeed())

			Expect(mix).Should(HaveLen(3))
			Expect(mix[0].Name).Should(Equal("replay"))
			Expect(mix[0].Count).Should(BeNumerically("==", 3))

			var total int64
			for _, m := range mix {
				total += m.Count
			}
			Expect(total).Should(BeNumerically("==", 6)) // real rows never counted
		})
	})

	Describe("DecoyTopDomains", func() {
		It("ranks fake domains by eTLD+1, excluding real rows", func() {
			items, err := reader.DecoyTopDomains(from(), to(), 10)
			Expect(err).Should(Succeed())

			Expect(items).Should(HaveLen(3))
			Expect(items[0].Name).Should(Equal("alpha.com"))
			Expect(items[0].Count).Should(BeNumerically("==", 3))
			for _, it := range items {
				Expect(it.Name).ShouldNot(Equal("real.org"))
			}
		})
	})

	Describe("DecoyBuckets", func() {
		It("splits decoy counts by source per time slot", func() {
			buckets, err := reader.DecoyBuckets(from(), to(), 3600)
			Expect(err).Should(Succeed())

			Expect(buckets).Should(HaveLen(2))
			Expect(buckets[0].TS).Should(Equal(hour1.Unix()))
			Expect(buckets[0].Counts).Should(HaveKeyWithValue("replay", int64(3)))
			Expect(buckets[0].Counts).Should(HaveKeyWithValue("list", int64(1)))
			Expect(buckets[1].TS).Should(Equal(hour2.Unix()))
			Expect(buckets[1].Counts).Should(HaveKeyWithValue("corpus", int64(2)))
		})

		It("falls back to one-hour slots for a non-positive step", func() {
			buckets, err := reader.DecoyBuckets(from(), to(), 0)
			Expect(err).Should(Succeed())
			Expect(buckets).Should(HaveLen(2))
		})
	})

	Describe("real-scoped stats stay decoy-free", func() {
		It("Overview counts only the real rows", func() {
			o, err := reader.Overview(from(), to())
			Expect(err).Should(Succeed())
			Expect(o.Queries).Should(BeNumerically("==", 2)) // only the 2 real rows aggregated
		})
	})
})
