//go:build !mips && !mipsle && !mips64 && !mips64le && !loong64 && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package querylog

import (
	"context"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"

	"github.com/glebarez/sqlite"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Write-path aggregates", func() {
	var (
		writer *DatabaseWriter
		hour1  time.Time
		hour2  time.Time
	)

	entry := func(start time.Time, client, question, responseType string, durationMs int64, decoy bool) *LogEntry {
		return &LogEntry{
			Start:        start,
			ClientIP:     "192.168.1.10",
			ClientNames:  []string{client},
			QuestionName: question,
			QuestionType: "A",
			ResponseType: responseType,
			DurationMs:   durationMs,
			Decoy:        decoy,
			Fingerprint:  model.Fingerprint{Transport: model.TransportDoT},
		}
	}

	BeforeEach(func() {
		ctx, cancelFn := context.WithCancel(context.Background())
		DeferCleanup(cancelFn)

		var err error
		writer, err = newDatabaseWriter(ctx, sqlite.Open("file::memory:"), 7, time.Minute, config.QueryLogTypeSqlite)
		Expect(err).Should(Succeed())

		sqlDB, err := writer.db.DB()
		Expect(err).Should(Succeed())
		sqlDB.SetMaxOpenConns(1)
		DeferCleanup(sqlDB.Close)

		hour1 = time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
		hour2 = hour1.Add(time.Hour)
	})

	It("maintains per-hour counts, latency histogram and domain aggregates", func() {
		writer.Write(entry(hour1.Add(1*time.Minute), "laptop", "www.example.com.", "RESOLVED", 5, false))
		writer.Write(entry(hour1.Add(2*time.Minute), "laptop", "www.example.com.", "RESOLVED", 30, false))
		writer.Write(entry(hour1.Add(3*time.Minute), "laptop", "cdn.example.com.", "RESOLVED", 120, false))
		writer.Write(entry(hour1.Add(4*time.Minute), "laptop", "ads.tracker.net.", "BLOCKED", 0, false))
		writer.Write(entry(hour2.Add(5*time.Minute), "phone", "www.example.com.", "RESOLVED", 2000, false))

		Expect(writer.doDBWrite()).Should(Succeed())

		var resolved aggHourly
		Expect(writer.db.Where("hour = ? AND client_name = ? AND response_type = ?",
			hour1, "laptop", "RESOLVED").First(&resolved).Error).Should(Succeed())
		Expect(resolved.Cnt).Should(BeNumerically("==", 3))
		Expect(resolved.SumDurationMs).Should(BeNumerically("==", 155))
		Expect(resolved.Transport).Should(Equal("dot"))
		Expect(resolved.Lat0_10).Should(BeNumerically("==", 1))
		Expect(resolved.Lat10_50).Should(BeNumerically("==", 1))
		Expect(resolved.Lat100_250).Should(BeNumerically("==", 1))
		Expect(resolved.Lat1000Inf).Should(BeNumerically("==", 0))

		var blocked aggHourly
		Expect(writer.db.Where("hour = ? AND response_type = ?", hour1, "BLOCKED").
			First(&blocked).Error).Should(Succeed())
		Expect(blocked.Cnt).Should(BeNumerically("==", 1))

		var hour2Row aggHourly
		Expect(writer.db.Where("hour = ? AND client_name = ?", hour2, "phone").
			First(&hour2Row).Error).Should(Succeed())
		Expect(hour2Row.Cnt).Should(BeNumerically("==", 1))
		Expect(hour2Row.Lat1000Inf).Should(BeNumerically("==", 1))

		var domain aggDomainHourly
		Expect(writer.db.Where("hour = ? AND etldp = ? AND blocked = ?", hour1, "example.com", false).
			First(&domain).Error).Should(Succeed())
		Expect(domain.Cnt).Should(BeNumerically("==", 3)) // www + www + cdn share the eTLD+1

		var blockedDomain aggDomainHourly
		Expect(writer.db.Where("blocked = ?", true).First(&blockedDomain).Error).Should(Succeed())
		Expect(blockedDomain.Etldp).Should(Equal("tracker.net"))
	})

	It("excludes decoy entries from aggregates but keeps them in the raw table", func() {
		writer.Write(entry(hour1, "laptop", "real.example.com.", "RESOLVED", 10, false))
		writer.Write(entry(hour1, "laptop", "decoy.example.com.", "RESOLVED", 10, true))

		Expect(writer.doDBWrite()).Should(Succeed())

		var rawCount int64
		Expect(writer.db.Model(&logEntry{}).Count(&rawCount).Error).Should(Succeed())
		Expect(rawCount).Should(BeNumerically("==", 2))

		var agg aggHourly
		Expect(writer.db.First(&agg).Error).Should(Succeed())
		Expect(agg.Cnt).Should(BeNumerically("==", 1))

		var aggCount int64
		Expect(writer.db.Model(&aggHourly{}).Count(&aggCount).Error).Should(Succeed())
		Expect(aggCount).Should(BeNumerically("==", 1))
	})

	It("accumulates into the same rows across two flushes (upsert)", func() {
		writer.Write(entry(hour1, "laptop", "www.example.com.", "RESOLVED", 30, false))
		Expect(writer.doDBWrite()).Should(Succeed())

		writer.Write(entry(hour1.Add(30*time.Minute), "laptop", "www.example.com.", "RESOLVED", 40, false))
		Expect(writer.doDBWrite()).Should(Succeed())

		var agg aggHourly
		Expect(writer.db.Where("hour = ?", hour1).First(&agg).Error).Should(Succeed())
		Expect(agg.Cnt).Should(BeNumerically("==", 2))
		Expect(agg.SumDurationMs).Should(BeNumerically("==", 70))
		Expect(agg.Lat10_50).Should(BeNumerically("==", 2))

		var aggCount int64
		Expect(writer.db.Model(&aggHourly{}).Count(&aggCount).Error).Should(Succeed())
		Expect(aggCount).Should(BeNumerically("==", 1))

		var domain aggDomainHourly
		Expect(writer.db.First(&domain).Error).Should(Succeed())
		Expect(domain.Cnt).Should(BeNumerically("==", 2))
	})
})
