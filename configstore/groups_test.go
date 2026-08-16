package configstore

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Household groups", func() {
	var store *Store

	BeforeEach(func() {
		var err error
		store, err = Open(GinkgoT().TempDir())
		Expect(err).Should(Succeed())
		DeferCleanup(store.Close)
	})

	It("composes a group's categories into its deny group and wires members", func() {
		Expect(store.SaveGroup("kids", []string{"porn", "gambling"})).Should(Succeed())
		Expect(store.SetGroupMembers("kids", []string{"kids-tablet"})).Should(Succeed())

		cfg, err := store.LoadConfig()
		Expect(err).Should(Succeed())

		froms := []string{}
		for _, src := range cfg.Blocking.Denylists["kids"] {
			froms = append(froms, src.From)
		}
		Expect(froms).Should(ConsistOf("blocklist:porn", "blocklist:gambling"))

		// members reference the group; the group is member-scoped, not global
		Expect(cfg.Blocking.ClientGroupsBlock["kids-tablet"]).Should(ContainElement("kids"))
		Expect(cfg.Blocking.ClientGroupsBlock["default"]).ShouldNot(ContainElement("kids"))
	})

	It("rejects an unknown category", func() {
		Expect(store.SaveGroup("kids", []string{"nope"})).
			Should(MatchError(ContainSubstring("unknown blocklist category")))
	})

	It("saves an empty enabled group with a member (no validate error) and the member inherits manual", func() {
		// a global manual entry, so the "manual" group exists to be inherited
		_, err := store.AddDenyEntry("manual", "bad.example.com", "")
		Expect(err).Should(Succeed())

		Expect(store.SaveGroup("guests", nil)).Should(Succeed())
		// the both-nil guard makes this savable: no categories, no allow/deny
		Expect(store.SetGroupMembers("guests", []string{"guest-phone"})).Should(Succeed())

		cfg, err := store.LoadConfig()
		Expect(err).Should(Succeed())

		// empty enabled group with a member gets an empty deny source so cfg.Validate passes
		Expect(cfg.Blocking.Denylists).Should(HaveKey("guests"))

		// the member joins cgb before the manual apply-to-everyone loop, so it inherits "manual"
		Expect(cfg.Blocking.ClientGroupsBlock["guest-phone"]).Should(ContainElements("guests", "manual"))
	})

	It("drops a disabled group from the overlay", func() {
		Expect(store.SaveGroup("kids", []string{"porn"})).Should(Succeed())
		Expect(store.SetGroupMembers("kids", []string{"kids-tablet"})).Should(Succeed())
		Expect(store.SetGroupEnabled("kids", false)).Should(Succeed())

		cfg, err := store.LoadConfig()
		Expect(err).Should(Succeed())
		Expect(cfg.Blocking.ClientGroupsBlock["kids-tablet"]).ShouldNot(ContainElement("kids"))
	})
})
