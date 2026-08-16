package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("LoadFromYAML", func() {
	suiteBeforeEach()

	When("input is empty", func() {
		It("returns the default configuration without error", func() {
			cfg, err := LoadFromYAML(nil)
			Expect(err).Should(Succeed())

			defaults, err := WithDefaults[Config]()
			Expect(err).Should(Succeed())
			Expect(*cfg).Should(Equal(defaults))
		})
	})

	When("input is a small valid config", func() {
		It("parses upstream group and port", func() {
			data := []byte(`
upstreams:
  groups:
    default:
      - 1.1.1.1
ports:
  dns: 5353
`)
			cfg, err := LoadFromYAML(data)
			Expect(err).Should(Succeed())
			Expect(cfg.Upstreams.Groups).Should(HaveKey("default"))
			Expect(cfg.Upstreams.Groups["default"]).Should(HaveLen(1))
			Expect(cfg.Ports.DNS).Should(Equal(ListenConfig{":5353"}))
		})
	})

	// Regression: a legacy/hand-edited blob with a local override zone AND an
	// explicit filterUnmappedTypes:false would leak unmapped types (AAAA/HTTPS/
	// SVCB) upstream. Load must pin it true whenever a zone is present.
	When("a customDNS zone is present with an explicit filterUnmappedTypes:false", func() {
		It("forces filterUnmappedTypes true so unmapped types are not leaked upstream", func() {
			data := []byte(`
customDNS:
  filterUnmappedTypes: false
  zone: |
    example.zone. 3600 IN A 1.2.3.4
`)
			cfg, err := LoadFromYAML(data)
			Expect(err).Should(Succeed())
			Expect(cfg.CustomDNS.Zone.RRs).Should(HaveKey("example.zone."))
			Expect(cfg.CustomDNS.FilterUnmappedTypes).Should(BeTrue())
		})
	})

	When("filterUnmappedTypes:false is set with only mappings (no zone)", func() {
		It("leaves the flag false (legitimate upstream-forward for simple mappings)", func() {
			data := []byte(`
customDNS:
  filterUnmappedTypes: false
  mapping:
    custom.domain: 1.2.3.4
`)
			cfg, err := LoadFromYAML(data)
			Expect(err).Should(Succeed())
			Expect(cfg.CustomDNS.FilterUnmappedTypes).Should(BeFalse())
		})
	})

	When("input has an unknown key", func() {
		It("returns an error (strict mode)", func() {
			_, err := LoadFromYAML([]byte("notARealKey: true\n"))
			Expect(err).Should(MatchError(ContainSubstring("wrong file structure")))
		})
	})

	When("validation fails", func() {
		// These configs previously called logger.Fatal and exited the process.
		It("returns an error for an invalid rate limit config", func() {
			_, err := LoadFromYAML([]byte("rateLimit:\n  enable: true\n"))
			Expect(err).Should(MatchError(ContainSubstring("rate must be > 0")))
		})

		It("returns an error for an invalid DNS64 prefix length", func() {
			data := []byte("dns64:\n  enable: true\n  prefixes:\n    - 64:ff9b::/95\n")
			_, err := LoadFromYAML(data)
			Expect(err).Should(MatchError(ContainSubstring("invalid length")))
		})
	})
})
