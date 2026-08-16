package lists

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/0xERR0R/blocky/config"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeStore is an in-memory Store: no sqlite, no cycle into querylog. It records
// the writes the updater makes so the version-gate behaviour can be asserted.
type fakeStore struct {
	meta          map[string]string   // "source|category" -> version
	decoyReplaced []string            // last decoy domains written
	decoyCalls    int                 // ReplaceDecoy invocations
	blReplaced    map[string][]string // category -> last domains written
	blSeeded      map[string]bool     // categories that have rows
	failDecoy     bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		meta:       map[string]string{},
		blReplaced: map[string][]string{},
		blSeeded:   map[string]bool{},
	}
}

func key(s, c string) string { return s + "|" + c }

func readLines(r io.Reader) []string {
	b, _ := io.ReadAll(r)
	var out []string
	for l := range strings.SplitSeq(string(b), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}

	return out
}

func (f *fakeStore) GetListMeta(source, category string) (string, error) {
	return f.meta[key(source, category)], nil
}

func (f *fakeStore) SetListMeta(source, category, version string) error {
	f.meta[key(source, category)] = version

	return nil
}

func (f *fakeStore) ReplaceDecoy(r io.Reader) (int, error) {
	f.decoyCalls++
	if f.failDecoy {
		return 0, errors.New("boom")
	}
	f.decoyReplaced = readLines(r)

	return len(f.decoyReplaced), nil
}

func (f *fakeStore) SeedBlocklistIfEmpty(category string, r io.Reader) (int, error) {
	if f.blSeeded[category] {
		return 0, nil
	}
	lines := readLines(r)
	f.blReplaced[category] = lines
	f.blSeeded[category] = true

	return len(lines), nil
}

func (f *fakeStore) ReplaceBlocklist(category string, r io.Reader) (int, error) {
	lines := readLines(r)
	f.blReplaced[category] = lines
	f.blSeeded[category] = true

	return len(lines), nil
}

func (f *fakeStore) PruneBlocklist(category string) error {
	delete(f.blReplaced, category)
	delete(f.blSeeded, category)
	delete(f.meta, key("blocklistproject", category))

	return nil
}

func makeTrancoCSV(domains []string) []byte {
	var buf bytes.Buffer
	for i, d := range domains {
		fmt.Fprintf(&buf, "%d,%s\n", i+1, d)
	}

	return buf.Bytes()
}

var _ = Describe("List updater", func() {
	var (
		store *fakeStore
		u     *Updater
		cfg   config.ListUpdaterConfig
	)

	BeforeEach(func() {
		store = newFakeStore()
		cfg = config.ListUpdaterConfig{
			Enable:        true,
			IntervalHours: 168,
			TrancoURL:     "https://tranco.example",
			BlocklistRepo: "blocklistproject/Lists",
		}
		u = NewUpdater(cfg, store, true)
	})

	Describe("normalizeDomain", func() {
		It("lowercases, trims dots, and drops junk", func() {
			Expect(normalizeDomain("  Example.COM. ")).To(Equal("example.com"))
			Expect(normalizeDomain("nodot")).To(BeEmpty())
			Expect(normalizeDomain("has space.com")).To(BeEmpty())
			Expect(normalizeDomain(".leading.com")).To(BeEmpty())
			Expect(normalizeDomain("")).To(BeEmpty())
		})
	})

	Describe("trancoCSVToDomains", func() {
		It("extracts and normalizes domains from the csv", func() {
			out, err := trancoCSVToDomains(makeTrancoCSV([]string{"A.com", "b.org", "bad"}))
			Expect(err).ToNot(HaveOccurred())
			Expect(strings.Fields(string(out))).To(Equal([]string{"a.com", "b.org"}))
		})
	})

	Describe("checkTranco version gate", func() {
		It("is a no-op when the stored id equals the latest id", func() {
			store.meta[key(sourceTranco, "")] = "ID1"
			u.get = func(_ context.Context, url string) ([]byte, error) {
				Expect(url).To(ContainSubstring("/api/lists/date/latest"))

				return []byte(`{"list_id":"ID1"}`), nil
			}

			u.checkTranco(context.Background())
			Expect(store.decoyCalls).To(Equal(0))
		})

		It("downloads and replaces when the id differs", func() {
			store.meta[key(sourceTranco, "")] = "ID1"
			u.get = func(_ context.Context, url string) ([]byte, error) {
				switch {
				case strings.Contains(url, "/api/lists/date/latest"):
					return []byte(`{"list_id":"ID2"}`), nil
				case strings.Contains(url, "/download/ID2/1000000"):
					return makeTrancoCSV([]string{"a.com", "b.com", "c.com"}), nil
				}

				return nil, fmt.Errorf("unexpected url %s", url)
			}

			u.checkTranco(context.Background())
			Expect(store.decoyCalls).To(Equal(1))
			Expect(store.decoyReplaced).To(Equal([]string{"a.com", "b.com", "c.com"}))
			Expect(store.meta[key(sourceTranco, "")]).To(Equal("ID2"))
		})

		It("keeps the stored version when the download fails", func() {
			store.meta[key(sourceTranco, "")] = "ID1"
			u.get = func(_ context.Context, url string) ([]byte, error) {
				if strings.Contains(url, "latest") {
					return []byte(`{"list_id":"ID2"}`), nil
				}

				return nil, errors.New("network down")
			}

			u.checkTranco(context.Background())
			Expect(store.decoyCalls).To(Equal(0))
			Expect(store.meta[key(sourceTranco, "")]).To(Equal("ID1")) // unchanged
		})
	})

	Describe("SeedBlocklistFloor", func() {
		It("seeds every embedded category when all are enabled", func() {
			cats, err := EmbeddedCategories()
			Expect(err).ToNot(HaveOccurred())
			Expect(cats).ToNot(BeEmpty())
			u.SetEnabledCategories(func() ([]string, error) { return cats, nil })

			Expect(u.SeedBlocklistFloor()).To(Succeed())

			embCommit, err := EmbeddedCommit()
			Expect(err).ToNot(HaveOccurred())

			for _, c := range cats {
				Expect(store.blSeeded[c]).To(BeTrue(), "category %s seeded", c)
				Expect(store.meta[key(sourceBlp, c)]).To(Equal(embCommit))
			}
		})

		It("seeds only enabled categories and prunes the rest", func() {
			u.SetEnabledCategories(func() ([]string, error) { return []string{"ads"}, nil })

			Expect(u.SeedBlocklistFloor()).To(Succeed())

			Expect(store.blSeeded["ads"]).To(BeTrue())
			// a category that exists in the embed but isn't enabled stays unseeded
			Expect(store.blSeeded["malware"]).To(BeFalse())
		})

		It("falls back to the default set when no provider is wired", func() {
			Expect(u.SeedBlocklistFloor()).To(Succeed())

			for _, c := range defaultSeedCategories {
				Expect(store.blSeeded[c]).To(BeTrue(), "default category %s seeded", c)
			}
		})
	})

	Describe("checkBlocklists version gate", func() {
		It("is a no-op when the repo commit equals the stored version", func() {
			cats, err := EmbeddedCategories()
			Expect(err).ToNot(HaveOccurred())
			u.SetEnabledCategories(func() ([]string, error) { return cats, nil })
			Expect(u.SeedBlocklistFloor()).To(Succeed())
			embCommit, _ := EmbeddedCommit()

			replacedBefore := len(store.blReplaced)
			u.get = func(_ context.Context, url string) ([]byte, error) {
				Expect(url).To(ContainSubstring("api.github.com"))

				return fmt.Appendf(nil, `{"sha":%q}`, embCommit), nil
			}

			u.checkBlocklists(context.Background())
			Expect(store.blReplaced).To(HaveLen(replacedBefore)) // nothing re-fetched
		})
	})

	Describe("refreshBlocklistCategory", func() {
		It("strips comments and blanks then replaces the category", func() {
			u.get = func(_ context.Context, _ string) ([]byte, error) {
				return []byte("a.com\n# comment\n\nb.com # trailing\n"), nil
			}

			Expect(u.refreshBlocklistCategory(context.Background(), "ads", "SHA9")).To(Succeed())
			Expect(store.blReplaced["ads"]).To(Equal([]string{"a.com", "b.com"}))
			Expect(store.meta[key(sourceBlp, "ads")]).To(Equal("SHA9"))
		})
	})
})
