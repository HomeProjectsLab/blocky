package lists

import (
	"context"
	"errors"
	"io"

	"github.com/0xERR0R/blocky/config"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type fakeProvider struct {
	domains map[string][]string
	err     error
}

func (f *fakeProvider) ForEachBlocklistDomain(category string, fn func(domain string) error) error {
	if f.err != nil {
		return f.err
	}

	for _, d := range f.domains[category] {
		if err := fn(d); err != nil {
			return err
		}
	}

	return nil
}

var _ = Describe("blocklist source adapter", func() {
	AfterEach(func() {
		SetBlocklistProvider(nil)
	})

	It("streams a category's domains through NewSourceOpener", func() {
		SetBlocklistProvider(&fakeProvider{domains: map[string][]string{
			"ads": {"ads1.example.com", "ads2.example.com"},
		}})

		src := config.NewBytesSources("blocklist:ads")[0]
		Expect(src.Type).Should(Equal(config.BytesSourceTypeFile))

		opener, err := NewSourceOpener("", src, nil)
		Expect(err).Should(Succeed())
		Expect(opener.String()).Should(Equal("blocklist:ads"))

		r, err := opener.Open(context.Background())
		Expect(err).Should(Succeed())
		DeferCleanup(r.Close)

		data, err := io.ReadAll(r)
		Expect(err).Should(Succeed())
		Expect(string(data)).Should(Equal("ads1.example.com\nads2.example.com\n"))
	})

	It("yields an empty stream for an unseeded category", func() {
		SetBlocklistProvider(&fakeProvider{})

		opener, err := NewSourceOpener("", config.NewBytesSources("blocklist:none")[0], nil)
		Expect(err).Should(Succeed())

		r, err := opener.Open(context.Background())
		Expect(err).Should(Succeed())

		data, err := io.ReadAll(r)
		Expect(err).Should(Succeed())
		Expect(data).Should(BeEmpty())
	})

	It("surfaces provider errors to the reader", func() {
		SetBlocklistProvider(&fakeProvider{err: errors.New("db exploded")})

		opener, err := NewSourceOpener("", config.NewBytesSources("blocklist:ads")[0], nil)
		Expect(err).Should(Succeed())

		r, err := opener.Open(context.Background())
		Expect(err).Should(Succeed())

		_, err = io.ReadAll(r)
		Expect(err).Should(MatchError(ContainSubstring("db exploded")))
	})

	It("fails to open when no provider is registered", func() {
		opener, err := NewSourceOpener("", config.NewBytesSources("blocklist:ads")[0], nil)
		Expect(err).Should(Succeed())

		_, err = opener.Open(context.Background())
		Expect(err).Should(MatchError(ContainSubstring("no provider registered")))
	})
})
