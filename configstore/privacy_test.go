package configstore

import (
	"sync"

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

	It("does not lose a concurrent SetPrivacy vs SetLocalDNSZone update", func() {
		p, err := store.GetPrivacy()
		Expect(err).Should(Succeed())
		p.Decoy.Enable = true
		p.Decoy.ActiveHoursStart = 9
		p.Decoy.ActiveHoursEnd = 21

		// two writers touching DIFFERENT sections concurrently; each rewrites the
		// whole document from its own snapshot, so without serialization one
		// section's change is clobbered (last full-document write wins).
		var wg sync.WaitGroup

		wg.Add(2)

		go func() {
			defer wg.Done()
			defer GinkgoRecover()
			Expect(store.SetPrivacy(p)).Should(Succeed())
		}()

		go func() {
			defer wg.Done()
			defer GinkgoRecover()
			Expect(store.SetLocalDNSZone("host.lan. 3600 IN A 10.0.0.5\n")).Should(Succeed())
		}()

		wg.Wait()

		got, err := store.GetPrivacy()
		Expect(err).Should(Succeed())
		Expect(got.Decoy.Enable).Should(BeTrue(), "privacy change survived the concurrent zone write")

		zone, err := store.GetLocalDNSZone()
		Expect(err).Should(Succeed())
		Expect(zone).Should(ContainSubstring("10.0.0.5"), "zone change survived the concurrent privacy write")
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

	It("applies defaults on a fresh GetPrivacy (decoy disabled, shadow on)", func() {
		p, err := store.GetPrivacy()
		Expect(err).Should(Succeed())
		Expect(p.Decoy.Enable).Should(BeFalse())
		// default rate/weights come from the load pipeline, not the zero value
		Expect(p.Decoy.QueriesPerMinute).Should(BeNumerically(">", 0))
		Expect(p.ShadowBlockedQueries).Should(BeTrue())
		Expect(p.Decoy.ActiveHoursEnd).Should(Equal(24))
	})

	It("preserves unrelated privacy fields across a partial SetPrivacy", func() {
		// first write: flip several fields
		p, err := store.GetPrivacy()
		Expect(err).Should(Succeed())
		p.TTLJitter.Enable = true
		p.TTLJitter.PercentPct = 30
		p.EDNSPadding.Enable = true
		p.QueryCaseRandomization = true
		Expect(store.SetPrivacy(p)).Should(Succeed())

		// second write: change ONLY the decoy hours, carrying the read-back struct
		p2, err := store.GetPrivacy()
		Expect(err).Should(Succeed())
		p2.Decoy.Enable = true
		p2.Decoy.ActiveHoursStart = 6
		p2.Decoy.ActiveHoursEnd = 23
		Expect(store.SetPrivacy(p2)).Should(Succeed())

		got, err := store.GetPrivacy()
		Expect(err).Should(Succeed())
		Expect(got.Decoy.ActiveHoursStart).Should(Equal(6))
		// the earlier fields survived
		Expect(got.TTLJitter.Enable).Should(BeTrue())
		Expect(got.TTLJitter.PercentPct).Should(Equal(uint(30)))
		Expect(got.EDNSPadding.Enable).Should(BeTrue())
		Expect(got.QueryCaseRandomization).Should(BeTrue())
	})

	It("leaves unrelated top-level sections intact after SetPrivacy", func() {
		p, err := store.GetPrivacy()
		Expect(err).Should(Succeed())
		p.EDNSPadding.Enable = true
		Expect(store.SetPrivacy(p)).Should(Succeed())

		cfg, err := store.LoadConfig()
		Expect(err).Should(Succeed())
		Expect(cfg.Upstreams.EffectiveStrategy("default")).
			Should(Equal(config.UpstreamStrategyRecursive))
		Expect(cfg.QueryLog.Type).Should(Equal(config.QueryLogTypeSqlite))
	})

	It("errors from GetPrivacy on a malformed stored blob", func() {
		Expect(store.db.Model(&configBlob{}).Where("id = 1").
			Update("yaml", "privacy: [unterminated").Error).Should(Succeed())

		_, err := store.GetPrivacy()
		Expect(err).Should(HaveOccurred())
	})

	It("errors from SetPrivacy on a malformed stored blob without writing", func() {
		Expect(store.db.Model(&configBlob{}).Where("id = 1").
			Update("yaml", "privacy: [unterminated").Error).Should(Succeed())

		p := config.PrivacyConfig{}
		err := store.SetPrivacy(p)
		Expect(err).Should(HaveOccurred())
		Expect(err.Error()).Should(ContainSubstring("can't parse stored config"))
	})

	It("round-trips repeatedly without drift (stable serialization)", func() {
		p, err := store.GetPrivacy()
		Expect(err).Should(Succeed())
		p.Decoy.Enable = true
		p.Decoy.ActiveHoursStart = 7
		p.Decoy.ActiveHoursEnd = 19
		Expect(store.SetPrivacy(p)).Should(Succeed())

		first, err := store.RawYAML()
		Expect(err).Should(Succeed())

		// re-writing the read-back value must not perturb the document
		p2, err := store.GetPrivacy()
		Expect(err).Should(Succeed())
		Expect(store.SetPrivacy(p2)).Should(Succeed())

		second, err := store.RawYAML()
		Expect(err).Should(Succeed())
		Expect(second).Should(Equal(first))
	})
})
