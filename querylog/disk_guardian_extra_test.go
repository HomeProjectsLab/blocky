package querylog

import (
	"context"
	"errors"
	"time"

	"github.com/0xERR0R/blocky/config"

	"github.com/glebarez/sqlite"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("disk guardian edge cases", func() {
	var writer *DatabaseWriter

	dgNewWriter := func() *DatabaseWriter {
		ctx, cancel := context.WithCancel(context.Background())
		DeferCleanup(cancel)

		w, err := newDatabaseWriter(ctx, sqlite.Open("file::memory:"), 7, time.Minute, config.QueryLogTypeSqlite)
		Expect(err).Should(Succeed())

		db, err := w.db.DB()
		Expect(err).Should(Succeed())
		db.SetMaxOpenConns(1)
		DeferCleanup(db.Close)

		return w
	}

	BeforeEach(func() {
		writer = dgNewWriter()
	})

	AfterEach(func() {
		freeFractionFn = freeFraction
	})

	Context("pruneOldest boundaries", func() {
		It("returns 0 on an empty table", func() {
			deleted, err := writer.pruneOldest(time.Now(), 1000)
			Expect(err).Should(Succeed())
			Expect(deleted).Should(BeNumerically("==", 0))
		})

		It("keeps a row whose ts equals the floor (strict < comparison)", func() {
			floor := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
			writer.Write(&LogEntry{Start: floor, QuestionName: "at-floor.example.com.", DurationMs: 1})
			Expect(writer.doDBWrite()).Should(Succeed())

			deleted, err := writer.pruneOldest(floor, 1000)
			Expect(err).Should(Succeed())
			Expect(deleted).Should(BeNumerically("==", 0)) // == floor is not < floor
			Expect(countLogEntries(writer.db)).Should(BeNumerically("==", 1))
		})

		It("prunes a row one instant older than the floor", func() {
			floor := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
			writer.Write(&LogEntry{Start: floor.Add(-time.Nanosecond), QuestionName: "below.example.com.", DurationMs: 1})
			Expect(writer.doDBWrite()).Should(Succeed())

			deleted, err := writer.pruneOldest(floor, 1000)
			Expect(err).Should(Succeed())
			Expect(deleted).Should(BeNumerically("==", 1))
		})

		It("honours the batch limit, deleting at most `limit` rows per call", func() {
			for i := range 5 {
				writer.Write(&LogEntry{Start: time.Now().Add(-10 * time.Hour).Add(time.Duration(i) * time.Second),
					QuestionName: "old.example.com.", DurationMs: 1})
			}
			Expect(writer.doDBWrite()).Should(Succeed())

			deleted, err := writer.pruneOldest(time.Now().Add(-time.Hour), 2)
			Expect(err).Should(Succeed())
			Expect(deleted).Should(BeNumerically("==", 2))
			Expect(countLogEntries(writer.db)).Should(BeNumerically("==", 3))
		})
	})

	Context("enforceDiskTarget via the freeFractionFn seam", func() {
		It("does nothing when there is no disk pressure (free >= target)", func() {
			freeFractionFn = func(string) (float64, error) { return 0.90, nil }

			writer.Write(&LogEntry{Start: time.Now().Add(-10 * time.Hour), QuestionName: "old.example.com.", DurationMs: 1})
			Expect(writer.doDBWrite()).Should(Succeed())

			writer.enforceDiskTarget(context.Background(), "/tmp")
			Expect(countLogEntries(writer.db)).Should(BeNumerically("==", 1)) // untouched
		})

		It("does nothing when the free-space probe errors (unsupported platform)", func() {
			freeFractionFn = func(string) (float64, error) { return 0, errors.New("unsupported") }

			writer.Write(&LogEntry{Start: time.Now().Add(-10 * time.Hour), QuestionName: "old.example.com.", DurationMs: 1})
			Expect(writer.doDBWrite()).Should(Succeed())

			writer.enforceDiskTarget(context.Background(), "/tmp")
			Expect(countLogEntries(writer.db)).Should(BeNumerically("==", 1)) // untouched
		})

		It("stops at the recent-data floor when only recent rows exist under pressure", func() {
			freeFractionFn = func(string) (float64, error) { return 0.05, nil } // permanent pressure

			writer.Write(&LogEntry{Start: time.Now(), QuestionName: "fresh.example.com.", DurationMs: 1})
			Expect(writer.doDBWrite()).Should(Succeed())

			// Must terminate (the deleted==0 break), not spin to the step bound, and
			// must not delete the protected recent row.
			writer.enforceDiskTarget(context.Background(), "/tmp")
			Expect(countLogEntries(writer.db)).Should(BeNumerically("==", 1))
		})

		It("prunes only rows older than the floor under sustained pressure", func() {
			freeFractionFn = func(string) (float64, error) { return 0.05, nil }

			writer.Write(&LogEntry{Start: time.Now().Add(-10 * time.Hour), QuestionName: "old.example.com.", DurationMs: 1})
			writer.Write(&LogEntry{Start: time.Now(), QuestionName: "new.example.com.", DurationMs: 1})
			Expect(writer.doDBWrite()).Should(Succeed())

			writer.enforceDiskTarget(context.Background(), "/tmp")
			Expect(countLogEntries(writer.db)).Should(BeNumerically("==", 1)) // only the old row goes
		})

		It("stops before touching the DB when ctx is already cancelled", func() {
			freeFractionFn = func(string) (float64, error) { return 0.05, nil }

			writer.Write(&LogEntry{Start: time.Now().Add(-10 * time.Hour), QuestionName: "old.example.com.", DurationMs: 1})
			Expect(writer.doDBWrite()).Should(Succeed())

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			writer.enforceDiskTarget(ctx, "/tmp")
			Expect(countLogEntries(writer.db)).Should(BeNumerically("==", 1)) // ctx guard fires first
		})
	})

	Context("CloseDB", func() {
		It("is idempotent: a second CloseDB does not error", func() {
			w := dgNewWriter()
			Expect(w.CloseDB()).Should(Succeed())
			Expect(w.CloseDB()).Should(Succeed()) // database/sql Close is idempotent
		})
	})
})
