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
