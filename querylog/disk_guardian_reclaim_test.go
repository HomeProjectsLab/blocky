//go:build !mips && !mipsle && !mips64 && !mips64le && !loong64 && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package querylog

import (
	"context"
	"path/filepath"
	"time"

	"github.com/0xERR0R/blocky/config"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// realFileWriter opens a DatabaseWriter backed by a real on-disk sqlite file
// (WAL DSN via newSQLiteDialector), the configuration the auto_vacuum finding is
// about — an in-memory ":memory:" db does not exercise the WAL header write that
// silently disables the auto_vacuum pragma.
func realFileWriter() *DatabaseWriter {
	ctx, cancel := context.WithCancel(context.Background())
	DeferCleanup(cancel)

	dir := GinkgoT().TempDir()
	dialector, err := newSQLiteDialector(filepath.Join(dir, "querylog.db"))
	Expect(err).Should(Succeed())

	writer, err := newDatabaseWriter(ctx, dialector, 7, time.Minute, config.QueryLogTypeSqlite)
	Expect(err).Should(Succeed())

	DeferCleanup(func() {
		if sqlDB, e := writer.db.DB(); e == nil {
			_ = sqlDB.Close()
		}
	})

	// The heavy sqlite maintenance (INCREMENTAL auto_vacuum apply + the etldp index)
	// now runs off the boot path in buildDeferredIndexes; these real-file tests assume
	// it has completed (auto_vacuum=INCREMENTAL so incremental_vacuum drains freed
	// pages), so wait for it.
	Eventually(func() int {
		var mode int
		writer.db.Raw("PRAGMA auto_vacuum").Scan(&mode)

		return mode
	}, "10s", "20ms").Should(Equal(2))

	return writer
}

var _ = Describe("disk guardian reclaim (real file / WAL)", func() {
	It("enables auto_vacuum=INCREMENTAL despite the WAL DSN", func() {
		writer := realFileWriter()

		var mode int
		Expect(writer.db.Raw("PRAGMA auto_vacuum").Scan(&mode).Error).Should(Succeed())
		// 2 = INCREMENTAL. Before the VACUUM-after-pragma fix this was 0 (NONE),
		// making every guardian incremental_vacuum a permanent no-op.
		Expect(mode).Should(Equal(2))
	})

	It("returns freed pages to the OS via incremental_vacuum after a prune", func() {
		writer := realFileWriter()

		for i := 0; i < 2000; i++ {
			writer.Write(&LogEntry{
				Start:        time.Now().Add(-10 * time.Hour),
				QuestionName: "old.example.com.",
				DurationMs:   5,
			})
		}

		Expect(writer.doDBWrite()).Should(Succeed())

		deleted, err := writer.pruneOldest(time.Now().Add(-time.Hour), 1_000_000)
		Expect(err).Should(Succeed())
		Expect(deleted).Should(BeNumerically(">=", 2000))

		var freeBefore int
		Expect(writer.db.Raw("PRAGMA freelist_count").Scan(&freeBefore).Error).Should(Succeed())
		Expect(freeBefore).Should(BeNumerically(">", 0)) // delete parked pages on the freelist

		writer.db.Exec("PRAGMA incremental_vacuum")

		var freeAfter int
		Expect(writer.db.Raw("PRAGMA freelist_count").Scan(&freeAfter).Error).Should(Succeed())
		// With auto_vacuum=NONE this is a no-op (freeAfter == freeBefore); with the
		// fix the freelist is drained back to the OS.
		Expect(freeAfter).Should(BeNumerically("<", freeBefore))
	})
})

var _ = Describe("disk guardian ctx handling", func() {
	var writer *DatabaseWriter

	BeforeEach(func() {
		writer = realFileWriter()
		writer.dbPath = filepath.Join(GinkgoT().TempDir(), "querylog.db")
	})

	AfterEach(func() {
		freeFractionFn = freeFraction
	})

	It("stops pruning immediately when its ctx is cancelled (retired bundle)", func() {
		// simulate disk pressure so enforceDiskTarget would otherwise prune
		freeFractionFn = func(string) (float64, error) { return 0.05, nil }

		writer.Write(&LogEntry{Start: time.Now().Add(-10 * time.Hour), QuestionName: "old.example.com.", DurationMs: 5})
		Expect(writer.doDBWrite()).Should(Succeed())

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // bundle already retired

		writer.enforceDiskTarget(ctx, filepath.Dir(writer.dbPath))

		// the ctx guard fires before pruneOldest: the old row is untouched
		Expect(countLogEntries(writer.db)).Should(BeNumerically("==", 1))
	})

	It("prunes under real pressure when ctx is live", func() {
		freeFractionFn = func(string) (float64, error) { return 0.05, nil }

		writer.Write(&LogEntry{Start: time.Now().Add(-10 * time.Hour), QuestionName: "old.example.com.", DurationMs: 5})
		writer.Write(&LogEntry{Start: time.Now(), QuestionName: "new.example.com.", DurationMs: 5})
		Expect(writer.doDBWrite()).Should(Succeed())

		writer.enforceDiskTarget(context.Background(), filepath.Dir(writer.dbPath))

		// only the old row (older than the 1h floor) is pruned; the recent row stays
		Expect(countLogEntries(writer.db)).Should(BeNumerically("==", 1))
	})
})
