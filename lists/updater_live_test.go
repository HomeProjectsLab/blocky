package lists

import (
	"context"
	"os"
	"time"

	"github.com/0xERR0R/blocky/config"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These hit the real Tranco / GitHub APIs. They are skipped unless
// BLOCKY_LIVE_TESTS is set, so CI and offline runs stay hermetic.
var _ = Describe("List updater (live)", Label("live"), func() {
	var u *Updater

	BeforeEach(func() {
		if os.Getenv("BLOCKY_LIVE_TESTS") == "" {
			Skip("set BLOCKY_LIVE_TESTS=1 to run live network tests")
		}

		u = NewUpdater(config.ListUpdaterConfig{
			TrancoURL:     "https://tranco-list.eu",
			BlocklistRepo: "blocklistproject/Lists",
		}, newFakeStore(), true)
	})

	It("resolves the latest Tranco list id", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		id, err := u.latestTrancoID(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(id).ToNot(BeEmpty())
	})

	It("resolves the latest blocklistproject commit sha", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		sha, err := u.latestBlocklistCommit(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(sha).To(HaveLen(40))
	})
})
