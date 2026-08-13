package configstore

import (
	"github.com/0xERR0R/blocky/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Store privacy", func() {
	var store *Store

	BeforeEach(func() {
		var err error
		store, err = Open(GinkgoT().TempDir())
		Expect(err).Should(Succeed())
		DeferCleanup(func() { Expect(store.Close()).Should(Succeed()) })
	})

	It("round-trips the privacy section, preserving the rest of the config", func() {
		p, err := store.GetPrivacy()
		Expect(err).Should(Succeed())
		Expect(p.Decoy.Enable).Should(BeFalse())

		p.Decoy.Enable = true
		p.Decoy.ActiveHoursStart = 9
		p.Decoy.ActiveHoursEnd = 21
		p.TTLJitter.Enable = true
		p.TTLJitter.PercentPct = 20

		Expect(store.SetPrivacy(p)).Should(Succeed())

		got, err := store.GetPrivacy()
		Expect(err).Should(Succeed())
		Expect(got.Decoy.Enable).Should(BeTrue())
		Expect(got.Decoy.ActiveHoursStart).Should(Equal(9))
		Expect(got.Decoy.ActiveHoursEnd).Should(Equal(21))
		Expect(got.TTLJitter.PercentPct).Should(Equal(uint(20)))

		// the rest of the seeded config survives the merge
		cfg, err := store.LoadConfig()
		Expect(err).Should(Succeed())
		Expect(cfg.Upstreams.EffectiveStrategy("default")).
			Should(Equal(config.UpstreamStrategyRecursive))
	})

	It("rejects an invalid privacy section without persisting", func() {
		bad := config.PrivacyConfig{}
		bad.Decoy.Enable = true
		bad.Decoy.ActiveHoursStart = 10
		bad.Decoy.ActiveHoursEnd = 10 // start >= end

		Expect(store.SetPrivacy(bad)).Should(HaveOccurred())

		got, err := store.GetPrivacy()
		Expect(err).Should(Succeed())
		Expect(got.Decoy.Enable).Should(BeFalse())
	})
})
