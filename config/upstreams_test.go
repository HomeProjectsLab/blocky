package config

import (
	"time"

	"github.com/creasty/defaults"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v2"
)

var _ = Describe("ParallelBestConfig", func() {
	suiteBeforeEach()

	Context("Upstreams", func() {
		var cfg Upstreams

		BeforeEach(func() {
			cfg = Upstreams{
				Timeout: Duration(5 * time.Second),
				Groups: UpstreamGroups{
					UpstreamDefaultCfgName: {
						{Host: "host1"},
						{Host: "host2"},
					},
				},
			}
		})

		Describe("IsEnabled", func() {
			It("should be false by default", func() {
				cfg := Upstreams{}
				Expect(defaults.Set(&cfg)).Should(Succeed())

				Expect(cfg.IsEnabled()).Should(BeFalse())
			})

			When("enabled", func() {
				It("should be true", func() {
					Expect(cfg.IsEnabled()).Should(BeTrue())
				})
			})

			When("disabled", func() {
				It("should be false", func() {
					cfg := Upstreams{}

					Expect(cfg.IsEnabled()).Should(BeFalse())
				})
			})
		})

		Describe("LogConfig", func() {
			It("should log configuration", func() {
				cfg.LogConfig(logger)

				Expect(hook.Calls).ShouldNot(BeEmpty())
				Expect(hook.Messages).Should(ContainElements(
					ContainSubstring("timeout:"),
					ContainSubstring("groups:"),
					ContainSubstring(":host2:"),
				))
			})

			When("QUIC upstream is configured", func() {
				It("should log QUIC configuration", func() {
					cfg.Groups = UpstreamGroups{
						UpstreamDefaultCfgName: {
							{Host: "dns.example.com", Net: NetProtocolQuic},
						},
					}
					cfg.QUIC = QUICConfig{
						MaxIdleTimeout:  Duration(30 * time.Second),
						KeepAlivePeriod: Duration(15 * time.Second),
					}

					cfg.LogConfig(logger)

					Expect(hook.Messages).Should(ContainElements(
						ContainSubstring("quic:"),
						ContainSubstring("maxIdleTimeout:"),
						ContainSubstring("keepAlivePeriod:"),
					))
				})
			})
		})

		Describe("validate", func() {
			It("should compute defaults", func() {
				cfg.Timeout = -1

				Expect(cfg.validate(logger)).Should(Succeed())

				Expect(cfg.Timeout).Should(BeNumerically(">", 0))

				Expect(hook.Calls).ShouldNot(BeEmpty())
				Expect(hook.Messages).Should(ContainElement(ContainSubstring("timeout")))
			})

			It("should not override valid user values", func() {
				Expect(cfg.validate(logger)).Should(Succeed())

				Expect(hook.Messages).ShouldNot(ContainElement(ContainSubstring("timeout")))
			})

			When("QUIC upstream is configured", func() {
				BeforeEach(func() {
					cfg.Groups = UpstreamGroups{
						UpstreamDefaultCfgName: {
							{Host: "dns.example.com", Net: NetProtocolQuic},
						},
					}
				})

				It("should warn when QUIC maxIdleTimeout is not above zero", func() {
					cfg.QUIC.MaxIdleTimeout = 0
					cfg.QUIC.KeepAlivePeriod = Duration(15 * time.Second)

					Expect(cfg.validate(logger)).Should(Succeed())

					Expect(cfg.QUIC.MaxIdleTimeout).Should(BeNumerically(">", 0))
					Expect(hook.Messages).Should(ContainElement(ContainSubstring("maxIdleTimeout")))
				})

				It("should warn when QUIC keepAlivePeriod is not above zero", func() {
					cfg.QUIC.MaxIdleTimeout = Duration(30 * time.Second)
					cfg.QUIC.KeepAlivePeriod = 0

					Expect(cfg.validate(logger)).Should(Succeed())

					Expect(cfg.QUIC.KeepAlivePeriod).Should(BeNumerically(">", 0))
					Expect(hook.Messages).Should(ContainElement(ContainSubstring("keepAlivePeriod")))
				})

				It("should warn when keepAlivePeriod >= maxIdleTimeout", func() {
					cfg.QUIC.MaxIdleTimeout = Duration(10 * time.Second)
					cfg.QUIC.KeepAlivePeriod = Duration(10 * time.Second)

					Expect(cfg.validate(logger)).Should(Succeed())

					Expect(hook.Messages).Should(ContainElement(ContainSubstring("keep-alive won't prevent idle timeout")))
				})
			})
		})
	})

	Context("UpstreamGroupConfig", func() {
		var cfg UpstreamGroup

		BeforeEach(func() {
			upstreamsCfg, err := WithDefaults[Upstreams]()
			Expect(err).Should(Succeed())

			cfg = NewUpstreamGroup("test", upstreamsCfg, []Upstream{
				{Host: "host1"},
				{Host: "host2"},
			})
		})

		Describe("IsEnabled", func() {
			It("should be false by default", func() {
				cfg := UpstreamGroup{}
				Expect(defaults.Set(&cfg)).Should(Succeed())

				Expect(cfg.IsEnabled()).Should(BeFalse())
			})

			When("enabled", func() {
				It("should be true", func() {
					Expect(cfg.IsEnabled()).Should(BeTrue())
				})
			})

			When("disabled", func() {
				It("should be false", func() {
					cfg := UpstreamGroup{}

					Expect(cfg.IsEnabled()).Should(BeFalse())
				})
			})
		})

		Describe("LogConfig", func() {
			It("should log configuration", func() {
				cfg.LogConfig(logger)

				Expect(hook.Calls).ShouldNot(BeEmpty())
				Expect(hook.Messages).Should(ContainElements(
					ContainSubstring("group: test"),
					ContainSubstring("upstreams:"),
					ContainSubstring(":host1:"),
					ContainSubstring(":host2:"),
				))
			})
		})
	})
})

var _ = Describe("UpstreamStrategy and group config", func() {
	suiteBeforeEach()

	Describe("UpstreamStrategy", func() {
		DescribeTable("parses all values from YAML",
			func(name string, expected UpstreamStrategy) {
				var cfg Upstreams

				Expect(yaml.Unmarshal([]byte("strategy: "+name), &cfg)).Should(Succeed())
				Expect(cfg.Strategy).Should(Equal(expected))
			},
			Entry("parallel_best", "parallel_best", UpstreamStrategyParallelBest),
			Entry("strict", "strict", UpstreamStrategyStrict),
			Entry("random", "random", UpstreamStrategyRandom),
			Entry("round_robin", "round_robin", UpstreamStrategyRoundRobin),
			Entry("weighted_round_robin", "weighted_round_robin", UpstreamStrategyWeightedRoundRobin),
			Entry("weighted_random", "weighted_random", UpstreamStrategyWeightedRandom),
			Entry("time_hop", "time_hop", UpstreamStrategyTimeHop),
			Entry("domain_shard", "domain_shard", UpstreamStrategyDomainShard),
			Entry("recursive", "recursive", UpstreamStrategyRecursive),
		)
	})

	Describe("GroupConfig", func() {
		It("parses groupConfig from YAML", func() {
			data := `
strategy: random
groupConfig:
  privacy:
    strategy: time_hop
    hopMin: 1m
    hopMax: 2m
`
			var cfg Upstreams

			Expect(yaml.Unmarshal([]byte(data), &cfg)).Should(Succeed())
			Expect(cfg.GroupConfig["privacy"].Strategy).Should(Equal(UpstreamStrategyTimeHop))
			Expect(cfg.GroupConfig["privacy"].HopMin).Should(Equal(Duration(1 * time.Minute)))
			Expect(cfg.GroupConfig["privacy"].HopMax).Should(Equal(Duration(2 * time.Minute)))
		})

		Describe("EffectiveStrategy", func() {
			It("falls back to the global strategy for unknown groups", func() {
				cfg := Upstreams{Strategy: UpstreamStrategyRandom}

				Expect(cfg.EffectiveStrategy("nope")).Should(Equal(UpstreamStrategyRandom))
			})

			It("falls back to the global strategy when the group's strategy is unset", func() {
				cfg := Upstreams{
					Strategy:    UpstreamStrategyStrict,
					GroupConfig: map[string]UpstreamGroupConfig{"g": {}},
				}

				Expect(cfg.EffectiveStrategy("g")).Should(Equal(UpstreamStrategyStrict))
			})

			It("uses the group's strategy override", func() {
				cfg := Upstreams{
					Strategy: UpstreamStrategyStrict,
					GroupConfig: map[string]UpstreamGroupConfig{
						"g": {Strategy: UpstreamStrategyDomainShard},
					},
				}

				Expect(cfg.EffectiveStrategy("g")).Should(Equal(UpstreamStrategyDomainShard))
			})
		})

		Describe("GroupSettings", func() {
			It("resolves hop defaults for absent groups", func() {
				cfg := Upstreams{Strategy: UpstreamStrategyTimeHop}

				gc := cfg.GroupSettings("g")
				Expect(gc.Strategy).Should(Equal(UpstreamStrategyTimeHop))
				Expect(gc.HopMin).Should(Equal(Duration(5 * time.Minute)))
				Expect(gc.HopMax).Should(Equal(Duration(30 * time.Minute)))
			})

			It("keeps explicit hop values", func() {
				cfg := Upstreams{
					GroupConfig: map[string]UpstreamGroupConfig{
						"g": {HopMin: Duration(time.Minute), HopMax: Duration(2 * time.Minute)},
					},
				}

				gc := cfg.GroupSettings("g")
				Expect(gc.HopMin).Should(Equal(Duration(time.Minute)))
				Expect(gc.HopMax).Should(Equal(Duration(2 * time.Minute)))
			})
		})

		Describe("validate", func() {
			var cfg Upstreams

			BeforeEach(func() {
				Expect(defaults.Set(&cfg)).Should(Succeed())
			})

			It("errors when hopMin > hopMax for time_hop", func() {
				cfg.GroupConfig = map[string]UpstreamGroupConfig{
					"g": {
						Strategy: UpstreamStrategyTimeHop,
						HopMin:   Duration(10 * time.Minute),
						HopMax:   Duration(1 * time.Minute),
					},
				}

				err := cfg.validate(logger)
				Expect(err).Should(HaveOccurred())
				Expect(err.Error()).Should(ContainSubstring("hopMin"))
			})

			It("errors when hopMin is negative for time_hop", func() {
				cfg.GroupConfig = map[string]UpstreamGroupConfig{
					"g": {
						Strategy: UpstreamStrategyTimeHop,
						HopMin:   Duration(-1 * time.Minute),
					},
				}

				Expect(cfg.validate(logger)).Should(HaveOccurred())
			})

			It("accepts time_hop with default hop values", func() {
				cfg.GroupConfig = map[string]UpstreamGroupConfig{
					"g": {Strategy: UpstreamStrategyTimeHop},
				}

				Expect(cfg.validate(logger)).Should(Succeed())
			})

			It("ignores hop values for non-time_hop strategies", func() {
				cfg.GroupConfig = map[string]UpstreamGroupConfig{
					"g": {
						Strategy: UpstreamStrategyRoundRobin,
						HopMin:   Duration(10 * time.Minute),
						HopMax:   Duration(1 * time.Minute),
					},
				}

				Expect(cfg.validate(logger)).Should(Succeed())
			})
		})
	})

	Describe("Upstream weight", func() {
		It("defaults to 0 from string form and EffectiveWeight resolves it to 1", func() {
			u, err := ParseUpstream("9.9.9.9")
			Expect(err).Should(Succeed())
			Expect(u.Weight).Should(Equal(uint(0)))
			Expect(u.EffectiveWeight()).Should(Equal(uint(1)))
		})

		It("returns the explicit weight", func() {
			u := Upstream{Host: "host1", Weight: 7}

			Expect(u.EffectiveWeight()).Should(Equal(uint(7)))
		})
	})
})
