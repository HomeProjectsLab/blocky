package configstore

import (
	"time"

	"github.com/0xERR0R/blocky/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Upstream tables", func() {
	var store *Store

	BeforeEach(func() {
		var err error
		store, err = Open(GinkgoT().TempDir())
		Expect(err).Should(Succeed())

		DeferCleanup(func() {
			Expect(store.Close()).Should(Succeed())
		})
	})

	Describe("LoadConfig overlay", func() {
		It("uses the blob's upstreams while the tables are empty", func() {
			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())
			Expect(cfg.Upstreams.Groups["default"]).Should(HaveLen(2))
			Expect(cfg.Upstreams.Groups["default"][0].Host).Should(Equal("9.9.9.9"))
		})

		It("replaces the blob's upstreams entirely once tables are non-empty", func() {
			Expect(store.PutUpstreamGroup(UpstreamGroup{Name: "default", Strategy: "round_robin"})).Should(Succeed())
			Expect(store.SetUpstreamEntries("default", []UpstreamEntry{
				{Address: "1.1.1.1", Weight: 3, Enabled: true},
				{Address: "8.8.8.8", Enabled: false},
				{Address: "tcp-tls:9.9.9.9", Enabled: true},
			})).Should(Succeed())

			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())

			group := cfg.Upstreams.Groups["default"]
			// disabled entry is skipped; blob's 9.9.9.9/149.112.112.112 are gone
			Expect(group).Should(HaveLen(2))
			Expect(group[0].Host).Should(Equal("1.1.1.1"))
			Expect(group[0].Weight).Should(Equal(uint(3)))
			Expect(group[1].Host).Should(Equal("9.9.9.9"))
			Expect(group[1].Net).Should(Equal(config.NetProtocolTcpTls))

			Expect(cfg.Upstreams.EffectiveStrategy("default")).Should(Equal(config.UpstreamStrategyRoundRobin))
		})

		It("allows a recursive group with zero entries", func() {
			Expect(store.PutUpstreamGroup(UpstreamGroup{Name: "default", Strategy: "recursive"})).Should(Succeed())

			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())
			Expect(cfg.Upstreams.Groups["default"]).Should(BeEmpty())
			Expect(cfg.Upstreams.EffectiveStrategy("default")).Should(Equal(config.UpstreamStrategyRecursive))
		})

		It("carries hop settings into groupConfig", func() {
			g := UpstreamGroup{
				Name:     "default",
				Strategy: "time_hop",
				HopMin:   int64(time.Minute),
				HopMax:   int64(10 * time.Minute),
			}
			Expect(store.PutUpstreamGroup(g)).Should(Succeed())
			Expect(store.SetUpstreamEntries("default", []UpstreamEntry{
				{Address: "1.1.1.1", Enabled: true},
				{Address: "9.9.9.9", Enabled: true},
			})).Should(Succeed())

			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())

			gc := cfg.Upstreams.GroupSettings("default")
			Expect(gc.Strategy).Should(Equal(config.UpstreamStrategyTimeHop))
			Expect(gc.HopMin.ToDuration()).Should(Equal(time.Minute))
			Expect(gc.HopMax.ToDuration()).Should(Equal(10 * time.Minute))
		})
	})

	Describe("PutUpstreamGroup", func() {
		It("rejects an unknown strategy without persisting", func() {
			err := store.PutUpstreamGroup(UpstreamGroup{Name: "default", Strategy: "quantum"})
			Expect(err).Should(HaveOccurred())

			groups, _, err := store.ListUpstreamGroups()
			Expect(err).Should(Succeed())
			Expect(groups).Should(BeEmpty())
		})

		It("rejects invalid hop settings for time_hop", func() {
			g := UpstreamGroup{
				Name:     "default",
				Strategy: "time_hop",
				HopMin:   int64(time.Hour),
				HopMax:   int64(time.Minute),
			}
			Expect(store.PutUpstreamGroup(g)).Should(MatchError(ContainSubstring("hopMin")))
		})

		It("upserts group meta", func() {
			Expect(store.PutUpstreamGroup(UpstreamGroup{Name: "default", Strategy: "round_robin"})).Should(Succeed())
			Expect(store.PutUpstreamGroup(UpstreamGroup{Name: "default", Strategy: "weighted_random"})).Should(Succeed())

			groups, _, err := store.ListUpstreamGroups()
			Expect(err).Should(Succeed())
			Expect(groups).Should(HaveLen(1))
			Expect(groups[0].Strategy).Should(Equal("weighted_random"))
		})
	})

	Describe("SetUpstreamEntries", func() {
		BeforeEach(func() {
			Expect(store.PutUpstreamGroup(UpstreamGroup{Name: "default"})).Should(Succeed())
		})

		It("rejects a garbage address and leaves the DB untouched", func() {
			Expect(store.SetUpstreamEntries("default", []UpstreamEntry{
				{Address: "1.1.1.1", Enabled: true},
			})).Should(Succeed())

			err := store.SetUpstreamEntries("default", []UpstreamEntry{
				{Address: "not a::valid///upstream", Enabled: true},
			})
			Expect(err).Should(HaveOccurred())

			_, entries, lerr := store.ListUpstreamGroups()
			Expect(lerr).Should(Succeed())
			Expect(entries["default"]).Should(HaveLen(1))
			Expect(entries["default"][0].Address).Should(Equal("1.1.1.1"))
		})

		It("rejects an unknown group", func() {
			err := store.SetUpstreamEntries("nope", []UpstreamEntry{{Address: "1.1.1.1", Enabled: true}})
			Expect(err).Should(MatchError(ContainSubstring("unknown upstream group")))
		})

		It("round-trips through ListUpstreamGroups with normalized positions", func() {
			Expect(store.SetUpstreamEntries("default", []UpstreamEntry{
				{Address: "1.1.1.1", Weight: 2, Enabled: true, Position: 99},
				{Address: "8.8.8.8", Enabled: false},
			})).Should(Succeed())

			groups, entries, err := store.ListUpstreamGroups()
			Expect(err).Should(Succeed())
			Expect(groups).Should(HaveLen(1))

			es := entries["default"]
			Expect(es).Should(HaveLen(2))
			Expect(es[0].Address).Should(Equal("1.1.1.1"))
			Expect(es[0].Position).Should(Equal(0))
			Expect(es[0].Weight).Should(Equal(uint(2)))
			Expect(es[0].Enabled).Should(BeTrue())
			Expect(es[1].Address).Should(Equal("8.8.8.8"))
			Expect(es[1].Position).Should(Equal(1))
			Expect(es[1].Enabled).Should(BeFalse())
		})
	})

	Describe("DeleteUpstreamGroup", func() {
		It("refuses to delete the default group", func() {
			Expect(store.DeleteUpstreamGroup("default")).
				Should(MatchError(ContainSubstring("can't be deleted")))
		})

		It("deletes a group and its entries", func() {
			Expect(store.PutUpstreamGroup(UpstreamGroup{Name: "default"})).Should(Succeed())
			Expect(store.SetUpstreamEntries("default", []UpstreamEntry{{Address: "1.1.1.1", Enabled: true}})).
				Should(Succeed())
			Expect(store.PutUpstreamGroup(UpstreamGroup{Name: "kids", Strategy: "strict"})).Should(Succeed())
			Expect(store.SetUpstreamEntries("kids", []UpstreamEntry{{Address: "9.9.9.9", Enabled: true}})).
				Should(Succeed())

			Expect(store.DeleteUpstreamGroup("kids")).Should(Succeed())

			groups, entries, err := store.ListUpstreamGroups()
			Expect(err).Should(Succeed())
			Expect(groups).Should(HaveLen(1))
			Expect(groups[0].Name).Should(Equal("default"))
			Expect(entries).ShouldNot(HaveKey("kids"))
		})
	})
})
