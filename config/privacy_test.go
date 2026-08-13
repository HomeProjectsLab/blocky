package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Privacy config", func() {
	Describe("defaults via LoadFromYAML", func() {
		It("applies documented decoy/jitter/padding defaults", func() {
			cfg, err := LoadFromYAML([]byte("privacy:\n  decoy:\n    enable: false\n"))
			Expect(err).Should(Succeed())

			p := cfg.Privacy
			Expect(p.Decoy.Enable).Should(BeFalse())
			Expect(p.Decoy.QueriesPerMinute).Should(Equal(float64(4)))
			Expect(p.Decoy.ReplayWeight).Should(Equal(uint(15)))
			Expect(p.Decoy.CorpusWeight).Should(Equal(uint(5)))
			Expect(p.Decoy.ListWeight).Should(Equal(uint(1)))
			Expect(p.Decoy.ActiveHoursStart).Should(Equal(0))
			Expect(p.Decoy.ActiveHoursEnd).Should(Equal(24))
			Expect(p.Decoy.RefreshURL).Should(BeEmpty())
			Expect(p.Decoy.DiurnalShaping).Should(BeTrue())
			Expect(p.Decoy.ReplayMutate).Should(BeTrue())
			Expect(p.Decoy.FingerprintMatch).Should(BeTrue())
			Expect(p.Decoy.SplitUpstream).Should(BeTrue())
			Expect(p.Decoy.MissChaffPct).Should(Equal(uint(15)))
			Expect(p.Decoy.ClusterPct).Should(Equal(uint(20)))
			Expect(p.Decoy.ShadowTTL).Should(BeTrue())
			Expect(p.Decoy.DualStackPct).Should(Equal(uint(55)))
			Expect(p.Decoy.OffHoursFloorQPM).Should(Equal(0.5))
			Expect(p.Decoy.ActiveHoursEdgeJitterMin).Should(Equal(30))
			Expect(p.TTLJitter.Enable).Should(BeFalse())
			Expect(p.TTLJitter.PercentPct).Should(Equal(uint(10)))
			Expect(p.EDNSPadding.Enable).Should(BeFalse())
		})
	})

	Describe("validate", func() {
		var p PrivacyConfig

		BeforeEach(func() {
			def, err := WithDefaults[PrivacyConfig]()
			Expect(err).Should(Succeed())
			p = def
		})

		It("passes with defaults (all disabled)", func() {
			Expect(p.validate(nil)).Should(Succeed())
		})

		It("passes when decoy enabled with valid active hours", func() {
			p.Decoy.Enable = true
			p.Decoy.ActiveHoursStart = 8
			p.Decoy.ActiveHoursEnd = 22
			Expect(p.validate(nil)).Should(Succeed())
		})

		It("rejects activeHoursStart out of range", func() {
			p.Decoy.Enable = true
			p.Decoy.ActiveHoursStart = 24
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("activeHoursStart")))
		})

		It("rejects activeHoursEnd out of range", func() {
			p.Decoy.Enable = true
			p.Decoy.ActiveHoursEnd = 25
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("activeHoursEnd")))
		})

		It("rejects start >= end", func() {
			p.Decoy.Enable = true
			p.Decoy.ActiveHoursStart = 10
			p.Decoy.ActiveHoursEnd = 10
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("must be <")))
		})

		It("rejects all weights zero when decoy enabled", func() {
			p.Decoy.Enable = true
			p.Decoy.ReplayWeight = 0
			p.Decoy.CorpusWeight = 0
			p.Decoy.ListWeight = 0
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("must not all be zero")))
		})

		It("ignores decoy misconfig when decoy disabled", func() {
			p.Decoy.Enable = false
			p.Decoy.ActiveHoursStart = 99
			p.Decoy.ReplayWeight = 0
			p.Decoy.CorpusWeight = 0
			p.Decoy.ListWeight = 0
			Expect(p.validate(nil)).Should(Succeed())
		})

		It("rejects missChaffPct above 100", func() {
			p.Decoy.Enable = true
			p.Decoy.MissChaffPct = 101
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("missChaffPct")))
		})

		It("rejects clusterPct above 100", func() {
			p.Decoy.Enable = true
			p.Decoy.ClusterPct = 101
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("clusterPct")))
		})

		It("rejects ttlJitter percent above 90", func() {
			p.TTLJitter.Enable = true
			p.TTLJitter.PercentPct = 91
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("percent")))
		})

		It("accepts ttlJitter percent at the 90 boundary", func() {
			p.TTLJitter.Enable = true
			p.TTLJitter.PercentPct = 90
			Expect(p.validate(nil)).Should(Succeed())
		})
	})
})
