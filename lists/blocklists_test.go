package lists

import (
	"bufio"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Embedded blocklists", func() {
	It("has a manifest with a commit and non-empty categories", func() {
		m, err := EmbeddedManifest()
		Expect(err).ToNot(HaveOccurred())
		Expect(m.Commit).ToNot(BeEmpty())
		Expect(m.Categories).ToNot(BeEmpty())

		for _, c := range m.Categories {
			Expect(c.Name).ToNot(BeEmpty())
			Expect(c.Domains).To(BeNumerically(">", 0), "category %s has domains", c.Name)
			Expect(c.Bytes).To(BeNumerically(">", 0))
		}
	})

	It("lists only categories whose gz is actually embedded", func() {
		cats, err := EmbeddedCategories()
		Expect(err).ToNot(HaveOccurred())
		Expect(cats).To(ContainElements("ads", "tracking")) // always shipped
	})

	It("opens a category and yields normalized domains", func() {
		r, err := OpenEmbeddedCategory("ads")
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()

		sc := bufio.NewScanner(r)
		Expect(sc.Scan()).To(BeTrue())
		Expect(sc.Text()).To(ContainSubstring(".")) // a domain
	})

	It("errors for an unknown category", func() {
		_, err := OpenEmbeddedCategory("does-not-exist")
		Expect(err).To(HaveOccurred())
	})
})
