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

var _ = Describe("Reader", func() {
	var (
		reader *Reader
		hour1  time.Time
		hour2  time.Time
	)

	entry := func(start time.Time, client, question, qtype, responseType string,
		durationMs int64, decoy bool,
	) *LogEntry {
		return &LogEntry{
			Start:        start,
			ClientIP:     "192.168.1.10",
			ClientNames:  []string{client},
			QuestionName: question,
			QuestionType: qtype,
			ResponseType: responseType,
			ResponseCode: "NOERROR",
			DurationMs:   durationMs,
			Decoy:        decoy,
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

		// hour1: laptop, 3 RESOLVED (30ms each), 1 BLOCKED, 1 CACHED; hour2: phone, 1 RESOLVED
		for i := range 3 {
			writer.Write(entry(hour1.Add(time.Duration(i)*time.Minute),
				"laptop", "www.example.com.", "A", "RESOLVED", 30, false))
		}
		writer.Write(entry(hour1.Add(10*time.Minute), "laptop", "ads.tracker.net.", "A", "BLOCKED", 0, false))
		writer.Write(entry(hour1.Add(11*time.Minute), "laptop", "www.example.com.", "AAAA", "CACHED", 0, false))
		writer.Write(entry(hour2.Add(1*time.Minute), "phone", "phone.example.org.", "A", "RESOLVED", 30, false))
		// decoy: raw only, never in stats
		writer.Write(entry(hour1.Add(12*time.Minute), "laptop", "decoy.example.com.", "A", "RESOLVED", 30, true))

		Expect(writer.Flush()).Should(Succeed())

		reader, err = NewReader(dbPath)
		Expect(err).Should(Succeed())
		DeferCleanup(reader.Close)
	})

	rangeFrom := func() time.Time { return hour1.Add(-time.Hour) }
	rangeTo := func() time.Time { return hour2.Add(time.Hour) }

	Describe("Overview", func() {
		It("aggregates counts, clients and latency over the range", func() {
			o, err := reader.Overview(rangeFrom(), rangeTo())
			Expect(err).Should(Succeed())

			Expect(o.Queries).Should(BeNumerically("==", 6)) // decoy excluded
			Expect(o.Blocked).Should(BeNumerically("==", 1))
			Expect(o.Cached).Should(BeNumerically("==", 1))
			Expect(o.Clients).Should(BeNumerically("==", 2))
			Expect(o.AvgMs).Should(BeNumerically("~", 120.0/6, 0.001))
			// 4x30ms in [10,50) and 2x0ms in [0,10): p95 interpolates inside [10,50)
			Expect(o.P95Ms).Should(BeNumerically(">", 10))
			Expect(o.P95Ms).Should(BeNumerically("<=", 50))
		})
	})

	Describe("Buckets", func() {
		It("returns hourly buckets keyed by response type", func() {
			buckets, err := reader.Buckets(rangeFrom(), rangeTo(), 3600)
			Expect(err).Should(Succeed())

			Expect(buckets).Should(HaveLen(2))
			Expect(buckets[0].TS).Should(Equal(hour1.Unix()))
			Expect(buckets[0].Counts).Should(HaveKeyWithValue("RESOLVED", int64(3)))
			Expect(buckets[0].Counts).Should(HaveKeyWithValue("BLOCKED", int64(1)))
			Expect(buckets[1].TS).Should(Equal(hour2.Unix()))
			Expect(buckets[1].Counts).Should(HaveKeyWithValue("RESOLVED", int64(1)))
		})

		It("clamps sub-hour steps to the stored hourly granularity", func() {
			buckets, err := reader.Buckets(rangeFrom(), rangeTo(), 60)
			Expect(err).Should(Succeed())
			Expect(buckets).Should(HaveLen(2))
		})

		It("resamples into larger steps", func() {
			buckets, err := reader.Buckets(rangeFrom(), rangeTo(), 24*3600)
			Expect(err).Should(Succeed())
			Expect(buckets).Should(HaveLen(1))

			var total int64
			for _, c := range buckets[0].Counts {
				total += c
			}
			Expect(total).Should(BeNumerically("==", 6))
		})
	})

	Describe("Top", func() {
		It("ranks domains", func() {
			items, err := reader.Top(rangeFrom(), rangeTo(), "domain", 10)
			Expect(err).Should(Succeed())
			Expect(items[0].Name).Should(Equal("example.com"))
			Expect(items[0].Count).Should(BeNumerically("==", 4))
		})

		It("ranks blocked domains only", func() {
			items, err := reader.Top(rangeFrom(), rangeTo(), "blocked", 10)
			Expect(err).Should(Succeed())
			Expect(items).Should(HaveLen(1))
			Expect(items[0].Name).Should(Equal("tracker.net"))
		})

		It("ranks clients", func() {
			items, err := reader.Top(rangeFrom(), rangeTo(), "client", 10)
			Expect(err).Should(Succeed())
			Expect(items[0].Name).Should(Equal("laptop"))
			Expect(items[0].Count).Should(BeNumerically("==", 5))
		})

		It("ranks transports", func() {
			items, err := reader.Top(rangeFrom(), rangeTo(), "transport", 10)
			Expect(err).Should(Succeed())
			Expect(items[0].Name).Should(Equal("do53-udp"))
			Expect(items[0].Count).Should(BeNumerically("==", 6))
		})

		It("rejects unknown columns", func() {
			_, err := reader.Top(rangeFrom(), rangeTo(), "nope", 10)
			Expect(err).Should(HaveOccurred())
		})
	})

	Describe("LatencyPercentiles", func() {
		It("interpolates within the histogram buckets", func() {
			// histogram: 2 in [0,10), 4 in [10,50) -> p50 target 3: 10 + (3-2)/4*40 = 20
			p, err := reader.LatencyPercentiles(rangeFrom(), rangeTo())
			Expect(err).Should(Succeed())
			Expect(p.P50).Should(BeNumerically("~", 20, 0.001))
			Expect(p.P99).Should(BeNumerically("~", 10+(5.94-2)/4*40, 0.001))
		})
	})

	Describe("Search", func() {
		It("returns items and total, excluding decoys by default", func() {
			total, items, err := reader.Search(SearchFilter{From: rangeFrom(), To: rangeTo()})
			Expect(err).Should(Succeed())
			Expect(total).Should(BeNumerically("==", 6))
			Expect(items).Should(HaveLen(6))
			// newest first
			Expect(items[0].Question).Should(Equal("phone.example.org"))
			Expect(items[0].ClientNames).Should(ConsistOf("phone"))
			Expect(items[0].Transport).Should(Equal("do53-udp"))
		})

		It("includes decoys on request", func() {
			total, _, err := reader.Search(SearchFilter{From: rangeFrom(), To: rangeTo(), IncludeDecoys: true})
			Expect(err).Should(Succeed())
			Expect(total).Should(BeNumerically("==", 7))
		})

		It("filters by client, domain, qtype and rtype", func() {
			total, _, err := reader.Search(SearchFilter{From: rangeFrom(), To: rangeTo(), Client: "phone"})
			Expect(err).Should(Succeed())
			Expect(total).Should(BeNumerically("==", 1))

			total, _, err = reader.Search(SearchFilter{From: rangeFrom(), To: rangeTo(), Domain: "tracker"})
			Expect(err).Should(Succeed())
			Expect(total).Should(BeNumerically("==", 1))

			total, _, err = reader.Search(SearchFilter{From: rangeFrom(), To: rangeTo(), Domain: "www.*"})
			Expect(err).Should(Succeed())
			Expect(total).Should(BeNumerically("==", 4))

			total, _, err = reader.Search(SearchFilter{From: rangeFrom(), To: rangeTo(), Qtype: "AAAA"})
			Expect(err).Should(Succeed())
			Expect(total).Should(BeNumerically("==", 1))

			total, _, err = reader.Search(SearchFilter{From: rangeFrom(), To: rangeTo(), Rtype: "BLOCKED"})
			Expect(err).Should(Succeed())
			Expect(total).Should(BeNumerically("==", 1))
		})

		It("paginates while keeping the full total", func() {
			total, items, err := reader.Search(SearchFilter{From: rangeFrom(), To: rangeTo(), Limit: 2, Offset: 2})
			Expect(err).Should(Succeed())
			Expect(total).Should(BeNumerically("==", 6))
			Expect(items).Should(HaveLen(2))
		})
	})

	Describe("TotalQueries", func() {
		It("counts all raw rows", func() {
			count, err := reader.TotalQueries()
			Expect(err).Should(Succeed())
			Expect(count).Should(BeNumerically("==", 7))
		})
	})

	Describe("NewReader", func() {
		It("fails for a missing database file", func() {
			_, err := NewReader(filepath.Join(GinkgoT().TempDir(), "missing.db"))
			Expect(err).Should(HaveOccurred())
		})
	})
})
