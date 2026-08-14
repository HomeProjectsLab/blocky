//go:build !mips && !mipsle && !mips64 && !mips64le && !(netbsd && !amd64) && !(openbsd && !amd64 && !arm64)

package querylog

import (
	"errors"
	"io"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// errReader yields data then fails, to drive a mid-repopulate transaction error.
type errReader struct {
	data []byte
	done bool
}

func (e *errReader) Read(p []byte) (int, error) {
	if !e.done {
		e.done = true
		n := copy(p, e.data)

		return n, nil
	}

	return 0, errors.New("simulated read failure")
}

var _ = Describe("Blocklist store", func() {
	var source *DecoySource

	BeforeEach(func() {
		path := filepath.Join(GinkgoT().TempDir(), "lists.db")

		var err error
		source, err = NewDecoySource(path)
		Expect(err).Should(Succeed())
		DeferCleanup(func() { _ = source.Close() })
	})

	Describe("seeding", func() {
		It("seeds a category once and is idempotent", func() {
			n, err := source.SeedBlocklistIfEmpty("ads", strings.NewReader("a.com\nb.com\nc.com\n"))
			Expect(err).Should(Succeed())
			Expect(n).To(Equal(3))

			n2, err := source.SeedBlocklistIfEmpty("ads", strings.NewReader("x.com\n"))
			Expect(err).Should(Succeed())
			Expect(n2).To(Equal(0)) // already populated

			cnt, err := source.BlocklistCount("ads")
			Expect(err).Should(Succeed())
			Expect(cnt).To(Equal(int64(3)))
		})

		It("dedups duplicate domains within a category", func() {
			//nolint:dupword // the repeated domain is exactly what this test feeds in
			n, err := source.ReplaceBlocklist("ads", strings.NewReader("dup.com\ndup.com\nother.com\n"))
			Expect(err).Should(Succeed())
			Expect(n).To(Equal(3)) // rows attempted

			cnt, _ := source.BlocklistCount("ads")
			Expect(cnt).To(Equal(int64(2))) // stored deduped by PK
		})
	})

	Describe("atomic replace", func() {
		It("keeps the previous rows when repopulate fails mid-stream", func() {
			_, err := source.SeedBlocklistIfEmpty("ads", strings.NewReader("keep1.com\nkeep2.com\n"))
			Expect(err).Should(Succeed())

			_, err = source.ReplaceBlocklist("ads", &errReader{data: []byte("new1.com\n")})
			Expect(err).To(HaveOccurred())

			cnt, err := source.BlocklistCount("ads")
			Expect(err).Should(Succeed())
			Expect(cnt).To(Equal(int64(2))) // rolled back to the seeded rows
		})

		It("replaces atomically on success", func() {
			_, _ = source.SeedBlocklistIfEmpty("ads", strings.NewReader("old.com\n"))

			n, err := source.ReplaceBlocklist("ads", strings.NewReader("new1.com\nnew2.com\n"))
			Expect(err).Should(Succeed())
			Expect(n).To(Equal(2))

			cnt, _ := source.BlocklistCount("ads")
			Expect(cnt).To(Equal(int64(2)))
		})

		It("rolls the decoy list back on failure", func() {
			_, err := source.SeedIfEmpty(strings.NewReader("d1.com\nd2.com\n"))
			Expect(err).Should(Succeed())

			_, err = source.ReplaceDecoy(&errReader{data: []byte("x.com\n")})
			Expect(err).To(HaveOccurred())

			d, err := source.SampleList()
			Expect(err).Should(Succeed())
			Expect(d).To(BeElementOf("d1.com", "d2.com")) // still populated
		})
	})

	Describe("read interface", func() {
		BeforeEach(func() {
			_, _ = source.ReplaceBlocklist("ads", strings.NewReader("a.com\nb.com\n"))
			_, _ = source.ReplaceBlocklist("tracking", strings.NewReader("t.com\n"))
		})

		It("reports categories with counts", func() {
			stats, err := source.BlocklistCategories()
			Expect(err).Should(Succeed())
			Expect(stats).To(HaveLen(2))

			counts := map[string]int64{}
			for _, s := range stats {
				counts[s.Name] = s.Count
			}
			Expect(counts["ads"]).To(Equal(int64(2)))
			Expect(counts["tracking"]).To(Equal(int64(1)))
		})

		It("streams every domain in a category", func() {
			var got []string
			err := source.ForEachBlocklistDomain("ads", func(d string) error {
				got = append(got, d)

				return nil
			})
			Expect(err).Should(Succeed())
			Expect(got).To(ConsistOf("a.com", "b.com"))
		})

		It("stops streaming on a callback error", func() {
			err := source.ForEachBlocklistDomain("ads", func(_ string) error {
				return io.ErrUnexpectedEOF
			})
			Expect(err).To(MatchError(io.ErrUnexpectedEOF))
		})

		It("samples a domain from the blocklist", func() {
			for range 20 {
				d, err := source.SampleBlocklist()
				Expect(err).Should(Succeed())
				Expect(d).To(BeElementOf("a.com", "b.com", "t.com"))
			}
		})
	})

	Describe("list_meta version gate", func() {
		It("returns empty for an unseen source and upserts", func() {
			v, err := source.GetListMeta("tranco", "")
			Expect(err).Should(Succeed())
			Expect(v).To(BeEmpty())

			Expect(source.SetListMeta("tranco", "", "ID1")).Should(Succeed())
			v, _ = source.GetListMeta("tranco", "")
			Expect(v).To(Equal("ID1"))

			Expect(source.SetListMeta("tranco", "", "ID2")).Should(Succeed())
			v, _ = source.GetListMeta("tranco", "")
			Expect(v).To(Equal("ID2")) // upsert overwrote
		})
	})
})
