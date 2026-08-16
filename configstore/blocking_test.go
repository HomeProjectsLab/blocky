package configstore

import (
	"strings"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/lists"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Blocking tables", func() {
	var store *Store

	BeforeEach(func() {
		var err error
		store, err = Open(GinkgoT().TempDir())
		Expect(err).Should(Succeed())
		DeferCleanup(store.Close)
	})

	Describe("category seeding", func() {
		It("seeds one row per embedded category with the default set pre-enabled", func() {
			cats, err := store.ListBlockingCategories()
			Expect(err).Should(Succeed())

			embedded, err := lists.EmbeddedCategories()
			Expect(err).Should(Succeed())
			Expect(cats).Should(HaveLen(len(embedded)))

			byName := map[string]BlockingCategory{}
			for _, c := range cats {
				byName[c.Name] = c
			}

			Expect(byName["ads"].Enabled).Should(BeTrue())
			Expect(byName["ads"].IsDefault).Should(BeTrue())
			Expect(byName["porn"].Enabled).Should(BeFalse())
			Expect(byName["gambling"].Enabled).Should(BeFalse())
		})

		It("does not re-seed on reopen after a toggle", func() {
			Expect(store.SetCategoryEnabled("ads", false)).Should(Succeed())
			Expect(store.Close()).Should(Succeed())

			reopened, err := Open(store.absDir)
			Expect(err).Should(Succeed())
			DeferCleanup(reopened.Close)

			cats, err := reopened.ListBlockingCategories()
			Expect(err).Should(Succeed())

			for _, c := range cats {
				if c.Name == "ads" {
					Expect(c.Enabled).Should(BeFalse())
				}
			}
		})
	})

	Describe("SetCategoryEnabled", func() {
		It("rejects an unknown category", func() {
			Expect(store.SetCategoryEnabled("nope", true)).
				Should(MatchError(ContainSubstring("unknown blocklist category")))
		})
	})

	Describe("segments", func() {
		It("replaces a client's categories and removes the segment when empty", func() {
			Expect(store.SetClientSegment("kids-tablet", []string{"porn", "gambling"})).Should(Succeed())
			Expect(store.SetClientSegment("kids-tablet", []string{"porn"})).Should(Succeed())

			segs, err := store.GetClientSegments()
			Expect(err).Should(Succeed())
			Expect(segs).Should(HaveKeyWithValue("kids-tablet", []string{"porn"}))

			Expect(store.SetClientSegment("kids-tablet", nil)).Should(Succeed())

			segs, err = store.GetClientSegments()
			Expect(err).Should(Succeed())
			Expect(segs).ShouldNot(HaveKey("kids-tablet"))
		})

		It("rejects unknown categories and empty clients", func() {
			Expect(store.SetClientSegment("kid", []string{"nope"})).
				Should(MatchError(ContainSubstring("unknown blocklist category")))
			Expect(store.SetClientSegment("  ", []string{"porn"})).
				Should(MatchError(ContainSubstring("client identifier is required")))
		})
	})

	Describe("manual allow/deny entries", func() {
		It("adds, lists and deletes entries", func() {
			id, err := store.AddDenyEntry("", "bad.example.com", "")
			Expect(err).Should(Succeed())

			id2, err := store.AddAllowEntry("manual", "good.example.com", "")
			Expect(err).Should(Succeed())

			denies, err := store.ListDenyEntries()
			Expect(err).Should(Succeed())
			Expect(denies).Should(HaveLen(1))
			Expect(denies[0].GroupName).Should(Equal("manual")) // empty group defaults
			Expect(denies[0].Domain).Should(Equal("bad.example.com"))

			Expect(store.DeleteDenyEntry(id)).Should(Succeed())
			Expect(store.DeleteAllowEntry(id2)).Should(Succeed())

			denies, err = store.ListDenyEntries()
			Expect(err).Should(Succeed())
			Expect(denies).Should(BeEmpty())
		})

		It("rejects garbage entries", func() {
			_, err := store.AddDenyEntry("manual", "", "")
			Expect(err).Should(HaveOccurred())

			_, err = store.AddDenyEntry("manual", "two words", "")
			Expect(err).Should(HaveOccurred())
		})
	})

	Describe("LoadConfig overlay", func() {
		It("builds denylists and clientGroupsBlock from the tables", func() {
			Expect(store.SetClientSegment("kids-tablet", []string{"porn"})).Should(Succeed())

			_, err := store.AddDenyEntry("manual", "bad.example.com", "")
			Expect(err).Should(Succeed())
			_, err = store.AddAllowEntry("manual", "good.example.com", "")
			Expect(err).Should(Succeed())

			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())

			// enabled default categories are backed by blocklist: sources
			Expect(cfg.Blocking.Denylists["ads"]).Should(HaveLen(1))
			Expect(cfg.Blocking.Denylists["ads"][0].From).Should(Equal("blocklist:ads"))
			Expect(cfg.Blocking.Denylists["ads"][0].Type).Should(Equal(config.BytesSourceTypeFile))

			// segment-only category gets a source even though globally disabled
			Expect(cfg.Blocking.Denylists["porn"]).Should(HaveLen(1))

			// manual entries land as inline text sources in their group
			Expect(cfg.Blocking.Denylists["manual"]).Should(HaveLen(1))
			Expect(cfg.Blocking.Denylists["manual"][0].From).Should(ContainSubstring("bad.example.com"))
			Expect(cfg.Blocking.Allowlists["manual"][0].From).Should(ContainSubstring("good.example.com"))

			// default clients get enabled categories + manual; segments get their own + manual
			Expect(cfg.Blocking.ClientGroupsBlock["default"]).Should(ContainElements("ads", "tracking", "manual"))
			Expect(cfg.Blocking.ClientGroupsBlock["default"]).ShouldNot(ContainElement("porn"))
			Expect(cfg.Blocking.ClientGroupsBlock["kids-tablet"]).Should(ConsistOf("porn", "manual"))

			Expect(cfg.Blocking.IsEnabled()).Should(BeTrue())
		})

		It("keeps blocking disabled when all categories are off and no entries exist", func() {
			cats, err := store.ListBlockingCategories()
			Expect(err).Should(Succeed())

			for _, c := range cats {
				if c.Enabled {
					Expect(store.SetCategoryEnabled(c.Name, false)).Should(Succeed())
				}
			}

			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())
			Expect(cfg.Blocking.ClientGroupsBlock).Should(BeEmpty())
			Expect(cfg.Blocking.IsEnabled()).Should(BeFalse())
		})

		It("skips the blocking overlay when the query log is not sqlite", func() {
			raw, err := store.RawYAML()
			Expect(err).Should(Succeed())

			// strip the queryLog section from the seeded blob
			Expect(store.SetRawYAML(stripQueryLog(raw))).Should(Succeed())

			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())
			Expect(cfg.Blocking.Denylists).Should(BeEmpty())
		})

		It("guards manual allow-only groups against allowlist-only mode", func() {
			_, err := store.AddAllowEntry("manual", "good.example.com", "")
			Expect(err).Should(Succeed())

			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())

			// an empty deny source must exist so "manual" is not allowlist-only
			Expect(cfg.Blocking.Denylists).Should(HaveKey("manual"))
		})
	})
})

func stripQueryLog(raw string) string {
	out := ""
	skip := false

	var outSb200 strings.Builder
	for _, line := range splitLines(raw) {
		if skip {
			if len(line) > 0 && line[0] != ' ' {
				skip = false
			} else {
				continue
			}
		}

		if line == "queryLog:" {
			skip = true

			continue
		}

		outSb200.WriteString(line + "\n")
	}
	out += outSb200.String()

	return out
}

func splitLines(s string) []string {
	var lines []string

	start := 0

	for i := range len(s) {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}

	if start < len(s) {
		lines = append(lines, s[start:])
	}

	return lines
}
