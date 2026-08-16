package config

import (
	"math"

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
			Expect(p.Decoy.CohortPct).Should(Equal(uint(55)))
			Expect(p.Decoy.SessionCoherence).Should(BeTrue())
			Expect(p.Decoy.StepPct).Should(Equal(uint(70)))
			Expect(p.Decoy.RevisitCadence).Should(BeTrue())
			Expect(p.Decoy.PersonaCover).Should(BeTrue())
			Expect(p.Decoy.TargetQPMPeak).Should(Equal(float64(40)))
			Expect(p.Decoy.TargetQPMTrough).Should(Equal(float64(6)))
			Expect(p.Decoy.ChatterPct).Should(Equal(uint(15)))
			Expect(p.Decoy.TCPPct).Should(Equal(uint(10)))
			Expect(p.Decoy.FailChaffPct).Should(Equal(uint(8)))
			Expect(p.Decoy.PersonaAttribution).Should(BeTrue())
			Expect(p.Decoy.AdaptiveBackoff).Should(BeTrue())
			Expect(p.Decoy.PersonaProfile).Should(Equal("auto"))
			Expect(p.Decoy.DeviceClass.Enable).Should(BeTrue())
			Expect(p.Decoy.DeviceClass.VendorTelemetry).Should(BeTrue())
			Expect(p.Decoy.DeviceClass.VendorFamilies).Should(BeEmpty())
			Expect(p.Decoy.DeviceClass.PhantomDevicesPct).Should(Equal(uint(20)))
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

		It("rejects a weight large enough to overflow int32 in the weight sum", func() {
			p.Decoy.Enable = true
			p.Decoy.ReplayWeight = 1 << 31 // int(sum) would go negative on 32-bit ARM
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("replayWeight")))
		})

		It("rejects unknown vendorFamilies entries", func() {
			p.Decoy.Enable = true
			p.Decoy.DeviceClass.VendorFamilies = []string{"apple"}
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("vendorFamilies")))
		})

		It("normalizes vendorFamilies case in place", func() {
			p.Decoy.Enable = true
			p.Decoy.DeviceClass.VendorFamilies = []string{" Sonos ", "TUYA"}
			Expect(p.validate(nil)).Should(Succeed())
			Expect(p.Decoy.DeviceClass.VendorFamilies).Should(Equal([]string{"sonos", "tuya"}))
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

		It("rejects cohortPct above 100", func() {
			p.Decoy.Enable = true
			p.Decoy.CohortPct = 101
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("cohortPct")))
		})

		It("rejects stepPct above 100", func() {
			p.Decoy.Enable = true
			p.Decoy.StepPct = 101
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("stepPct")))
		})

		It("rejects chatterPct above 100", func() {
			p.Decoy.Enable = true
			p.Decoy.ChatterPct = 101
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("chatterPct")))
		})

		It("rejects tcpPct above 100", func() {
			p.Decoy.Enable = true
			p.Decoy.TCPPct = 101
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("tcpPct")))
		})

		It("rejects failChaffPct above 100", func() {
			p.Decoy.Enable = true
			p.Decoy.FailChaffPct = 101
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("failChaffPct")))
		})

		It("rejects targetQpmPeak below targetQpmTrough", func() {
			p.Decoy.Enable = true
			p.Decoy.TargetQPMTrough = 40
			p.Decoy.TargetQPMPeak = 10
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("targetQpmPeak")))
		})

		It("rejects non-finite QPM values", func() {
			p.Decoy.Enable = true
			p.Decoy.TargetQPMPeak = math.NaN()
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("finite")))

			p.Decoy.TargetQPMPeak = 40
			p.Decoy.QueriesPerMinute = math.Inf(1)
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("finite")))
		})

		It("rejects negative queriesPerMinute", func() {
			p.Decoy.Enable = true
			p.Decoy.QueriesPerMinute = -1
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("queriesPerMinute")))
		})

		It("rejects an unknown personaProfile", func() {
			p.Decoy.Enable = true
			p.Decoy.PersonaProfile = "datacenter"
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("personaProfile")))
		})

		It("rejects deviceClass.phantomDevicesPct above 100", func() {
			p.Decoy.Enable = true
			p.Decoy.DeviceClass.PhantomDevicesPct = 101
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("phantomDevicesPct")))
		})

		It("applies the enterprise persona preset when targets are left at home defaults", func() {
			p.Decoy.PersonaProfile = "enterprise"
			peak, trough := p.Decoy.EffectivePersonaCurve()
			Expect(peak).Should(Equal(float64(300)))
			Expect(trough).Should(Equal(float64(60)))
		})

		It("lets explicit targets override the persona preset", func() {
			p.Decoy.PersonaProfile = "enterprise"
			p.Decoy.TargetQPMPeak = 120
			p.Decoy.TargetQPMTrough = 20
			peak, trough := p.Decoy.EffectivePersonaCurve()
			Expect(peak).Should(Equal(float64(120)))
			Expect(trough).Should(Equal(float64(20)))
		})

		It("keeps the home curve for the auto profile", func() {
			peak, trough := p.Decoy.EffectivePersonaCurve()
			Expect(peak).Should(Equal(float64(40)))
			Expect(trough).Should(Equal(float64(6)))
		})

		It("rejects a prewarm interval that would panic time.NewTicker", func() {
			p.Decoy.Enable = true
			p.Decoy.PrewarmEnable = true

			p.Decoy.PrewarmIntervalHours = 0 // NewTicker panics on a non-positive interval
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("prewarmIntervalHours")))

			p.Decoy.PrewarmIntervalHours = MaxIntervalHours + 1 // overflows into a negative duration
			Expect(p.validate(nil)).Should(MatchError(ContainSubstring("prewarmIntervalHours")))

			p.Decoy.PrewarmIntervalHours = 12
			Expect(p.validate(nil)).Should(Succeed())
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
