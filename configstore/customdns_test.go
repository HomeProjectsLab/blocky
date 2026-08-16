package configstore

import (
	"sync"
	"time"

	"github.com/0xERR0R/blocky/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// cdnsSeedWithCustomDNS overwrites the store's blob with the seed config plus a
// fully-populated customDNS section (customTTL / filterUnmappedTypes / mapping /
// zone) so preservation of the untouched keys can be asserted.
func cdnsSeedWithCustomDNS(store *Store) {
	raw, err := store.RawYAML()
	Expect(err).Should(Succeed())

	withCustomDNS := raw + `customDNS:
  customTTL: 2h
  filterUnmappedTypes: false
  mapping:
    printer.lan: 10.0.0.9
  zone: |
    old.lan. 3600 IN A 10.0.0.1
`
	Expect(store.SetRawYAML(withCustomDNS)).Should(Succeed())
}

var _ = Describe("Store customDNS zone", func() {
	var store *Store

	BeforeEach(func() {
		var err error
		store, err = Open(GinkgoT().TempDir())
		Expect(err).Should(Succeed())
		DeferCleanup(func() { Expect(store.Close()).Should(Succeed()) })
	})

	Context("GetLocalDNSZone", func() {
		It("returns empty string when the customDNS key is absent (seed config)", func() {
			zone, err := store.GetLocalDNSZone()
			Expect(err).Should(Succeed())
			Expect(zone).Should(BeEmpty())
		})

		It("returns empty string when the stored blob is empty", func() {
			// inject an empty blob directly (bypasses validation)
			Expect(store.conn().Model(&configBlob{}).Where("id = 1").
				Update("yaml", "").Error).Should(Succeed())

			zone, err := store.GetLocalDNSZone()
			Expect(err).Should(Succeed())
			Expect(zone).Should(BeEmpty())
		})

		It("returns empty string when customDNS exists but has no zone key", func() {
			raw, err := store.RawYAML()
			Expect(err).Should(Succeed())
			Expect(store.SetRawYAML(raw + "customDNS:\n  customTTL: 30m\n")).Should(Succeed())

			zone, err := store.GetLocalDNSZone()
			Expect(err).Should(Succeed())
			Expect(zone).Should(BeEmpty())
		})

		It("returns empty string when zone is present but not a string (malformed shape)", func() {
			raw, err := store.RawYAML()
			Expect(err).Should(Succeed())
			// zone as an empty mapping - assertion to string fails, must not panic
			bad := raw + "customDNS:\n  zone: \"\"\n"
			Expect(store.SetRawYAML(bad)).Should(Succeed())

			zone, err := store.GetLocalDNSZone()
			Expect(err).Should(Succeed())
			Expect(zone).Should(BeEmpty())
		})

		It("reads back an existing zone", func() {
			cdnsSeedWithCustomDNS(store)

			zone, err := store.GetLocalDNSZone()
			Expect(err).Should(Succeed())
			Expect(zone).Should(ContainSubstring("old.lan."))
			Expect(zone).Should(ContainSubstring("10.0.0.1"))
		})

		It("errors on a malformed stored blob", func() {
			Expect(store.conn().Model(&configBlob{}).Where("id = 1").
				Update("yaml", "customDNS: [unterminated").Error).Should(Succeed())

			_, err := store.GetLocalDNSZone()
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).Should(ContainSubstring("can't parse stored config"))
		})
	})

	Context("SetLocalDNSZone", func() {
		It("creates the customDNS section when absent and persists the zone", func() {
			Expect(store.SetLocalDNSZone("new.lan. 3600 IN A 10.0.0.7\n")).Should(Succeed())

			zone, err := store.GetLocalDNSZone()
			Expect(err).Should(Succeed())
			Expect(zone).Should(ContainSubstring("10.0.0.7"))

			// parses through the full pipeline into a real zone record
			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())
			Expect(cfg.CustomDNS.Zone.RRs).Should(HaveKey("new.lan."))
		})

		It("preserves customTTL / filterUnmappedTypes / mapping when only writing zone", func() {
			cdnsSeedWithCustomDNS(store)

			Expect(store.SetLocalDNSZone("host.lan. 3600 IN A 10.0.0.5\n")).Should(Succeed())

			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())

			Expect(cfg.CustomDNS.CustomTTL.ToDuration()).Should(Equal(2 * time.Hour))
			Expect(cfg.CustomDNS.FilterUnmappedTypes).Should(BeFalse())
			Expect(cfg.CustomDNS.Mapping).Should(HaveKey("printer.lan"))

			// zone was replaced, not appended
			zone, err := store.GetLocalDNSZone()
			Expect(err).Should(Succeed())
			Expect(zone).Should(ContainSubstring("host.lan."))
			Expect(zone).Should(ContainSubstring("10.0.0.5"))
			Expect(zone).ShouldNot(ContainSubstring("old.lan."))
		})

		It("preserves unrelated top-level sections (upstreams, ports)", func() {
			Expect(store.SetLocalDNSZone("a.lan. 3600 IN A 10.0.0.2\n")).Should(Succeed())

			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())
			Expect(cfg.Ports.HTTP).Should(ContainElement(":4000"))
			Expect(cfg.Upstreams.EffectiveStrategy("default")).
				Should(Equal(config.UpstreamStrategyRecursive))
		})

		It("round-trips a multi-line zone verbatim and is stable across repeated writes", func() {
			zoneText := "one.lan. 3600 IN A 10.0.0.1\ntwo.lan. 3600 IN A 10.0.0.2\n"

			Expect(store.SetLocalDNSZone(zoneText)).Should(Succeed())
			got1, err := store.GetLocalDNSZone()
			Expect(err).Should(Succeed())
			Expect(got1).Should(Equal(zoneText))

			// writing the same value again yields the same result (idempotent)
			Expect(store.SetLocalDNSZone(got1)).Should(Succeed())
			got2, err := store.GetLocalDNSZone()
			Expect(err).Should(Succeed())
			Expect(got2).Should(Equal(zoneText))
		})

		It("clears the zone when given an empty string", func() {
			cdnsSeedWithCustomDNS(store)

			Expect(store.SetLocalDNSZone("")).Should(Succeed())

			zone, err := store.GetLocalDNSZone()
			Expect(err).Should(Succeed())
			Expect(zone).Should(BeEmpty())

			// the mapping still survives - only zone was touched
			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())
			Expect(cfg.CustomDNS.Mapping).Should(HaveKey("printer.lan"))
		})

		It("rejects an invalid zone without persisting", func() {
			cdnsSeedWithCustomDNS(store)

			err := store.SetLocalDNSZone("this is not a valid zone record !!!\n")
			Expect(err).Should(HaveOccurred())

			// old zone is untouched
			zone, err := store.GetLocalDNSZone()
			Expect(err).Should(Succeed())
			Expect(zone).Should(ContainSubstring("old.lan."))
		})

		It("errors on a malformed stored blob and writes nothing", func() {
			Expect(store.conn().Model(&configBlob{}).Where("id = 1").
				Update("yaml", "customDNS: [unterminated").Error).Should(Succeed())

			err := store.SetLocalDNSZone("x.lan. 3600 IN A 10.0.0.8\n")
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).Should(ContainSubstring("can't parse stored config"))
		})
	})

	Context("nestedMap helper", func() {
		It("normalizes a map[any]any (yaml.v2 nested decode)", func() {
			out := nestedMap(map[any]any{"zone": "z", 42: "dropped-non-string-key"})
			Expect(out).Should(HaveKeyWithValue("zone", "z"))
			Expect(out).ShouldNot(HaveKey(42))
		})

		It("passes through a map[string]any unchanged", func() {
			in := map[string]any{"zone": "z"}
			Expect(nestedMap(in)).Should(HaveKeyWithValue("zone", "z"))
		})

		It("returns nil for a non-map value", func() {
			Expect(nestedMap("not a map")).Should(BeNil())
			Expect(nestedMap(nil)).Should(BeNil())
		})
	})

	// A real concurrency test: hammer both section writers together under -race.
	// The Store.mu lock must serialize the read-modify-write so neither the
	// zone nor the privacy change is clobbered by the other's full-document write.
	It("survives repeated concurrent SetLocalDNSZone / SetPrivacy without clobber", func() {
		p, err := store.GetPrivacy()
		Expect(err).Should(Succeed())
		p.Decoy.Enable = true
		p.Decoy.ActiveHoursStart = 8
		p.Decoy.ActiveHoursEnd = 22

		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
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
		}
		wg.Wait()

		// both sections must be present in the final document
		got, err := store.GetPrivacy()
		Expect(err).Should(Succeed())
		Expect(got.Decoy.Enable).Should(BeTrue())

		zone, err := store.GetLocalDNSZone()
		Expect(err).Should(Succeed())
		Expect(zone).Should(ContainSubstring("10.0.0.5"))
	})
})
