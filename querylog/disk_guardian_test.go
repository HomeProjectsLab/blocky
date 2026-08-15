package querylog

import (
	"context"
	"time"

	"github.com/0xERR0R/blocky/config"

	"github.com/glebarez/sqlite"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gorm.io/gorm"
)

func countAggHourly(db *gorm.DB) int64 {
	var n int64
	db.Raw("SELECT COUNT(*) FROM agg_hourly").Scan(&n)

	return n
}

var _ = Describe("disk guardian pruneOldest", func() {
	var writer *DatabaseWriter

	BeforeEach(func() {
		ctx, cancel := context.WithCancel(context.Background())
		DeferCleanup(cancel)

		var err error
		writer, err = newDatabaseWriter(ctx, sqlite.Open("file::memory:"), 7, time.Minute, config.QueryLogTypeSqlite)
		Expect(err).Should(Succeed())

		db, err := writer.db.DB()
		Expect(err).Should(Succeed())
		db.SetMaxOpenConns(1)
		DeferCleanup(db.Close)
	})

	It("deletes only raw rows older than the floor and keeps aggregate stats", func() {
		// one 10h-old row, one recent row
		writer.Write(&LogEntry{Start: time.Now().Add(-10 * time.Hour), QuestionName: "old.example.com.", DurationMs: 5})
		writer.Write(&LogEntry{Start: time.Now().Add(-10 * time.Minute), QuestionName: "new.example.com.", DurationMs: 5})
		Expect(writer.doDBWrite()).Should(Succeed())

		Expect(countLogEntries(writer.db)).Should(BeNumerically("==", 2))
		aggBefore := countAggHourly(writer.db)
		Expect(aggBefore).Should(BeNumerically(">=", 1))

		// prune everything older than 1h -> only the 10h row qualifies
		deleted, err := writer.pruneOldest(time.Now().Add(-time.Hour), 1000)
		Expect(err).Should(Succeed())
		Expect(deleted).Should(BeNumerically("==", 1))

		Expect(countLogEntries(writer.db)).Should(BeNumerically("==", 1))        // recent row kept
		Expect(countAggHourly(writer.db)).Should(BeNumerically("==", aggBefore)) // stats preserved
	})

	It("prunes nothing when all rows are newer than the floor", func() {
		writer.Write(&LogEntry{Start: time.Now(), QuestionName: "fresh.example.com.", DurationMs: 5})
		Expect(writer.doDBWrite()).Should(Succeed())

		deleted, err := writer.pruneOldest(time.Now().Add(-time.Hour), 1000)
		Expect(err).Should(Succeed())
		Expect(deleted).Should(BeNumerically("==", 0))
		Expect(countLogEntries(writer.db)).Should(BeNumerically("==", 1))
	})
})
