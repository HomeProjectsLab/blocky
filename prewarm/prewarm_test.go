package prewarm

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/0xERR0R/blocky/config"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestPrewarm(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Prewarm Suite")
}

// fakeAdder records every AddToCorpus call; no sqlite needed.
type fakeAdder struct {
	mu    sync.Mutex
	added []string
}

func (f *fakeAdder) AddToCorpus(domain string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, domain)

	return nil
}

func (f *fakeAdder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.added)
}

var _ = Describe("Prewarm worker", func() {
	var adder *fakeAdder

	baseCfg := func() config.DecoyConfig {
		return config.DecoyConfig{PrewarmEnable: true, PrewarmIntervalHours: 12}
	}

	BeforeEach(func() {
		adder = &fakeAdder{}
	})

	It("is nil (no-op) when disabled", func() {
		cfg := baseCfg()
		cfg.PrewarmEnable = false
		Expect(New(cfg, adder)).To(BeNil())
	})

	It("is nil when there is no corpus to write to", func() {
		Expect(New(baseCfg(), nil)).To(BeNil())
	})

	It("adds every domain a fetcher returns on one tick", func() {
		cfg := baseCfg()
		cfg.PrewarmURL = "http://trending.example/list"
		w := New(cfg, adder)
		w.get = func(_ context.Context, _ string) ([]byte, error) {
			return []byte("1,alpha.example\n2,beta.example\n\n# comment\ngamma.example\n"), nil
		}

		w.tick(context.Background())

		Expect(adder.added).To(Equal([]string{"alpha.example", "beta.example", "gamma.example"}))
	})

	It("respects the bounded per-run count", func() {
		cfg := baseCfg()
		cfg.PrewarmURL = "http://trending.example/list"
		w := New(cfg, adder)
		w.perRun = 3
		w.get = func(_ context.Context, _ string) ([]byte, error) {
			return []byte("a.example\nb.example\nc.example\nd.example\ne.example\n"), nil
		}

		w.tick(context.Background())

		Expect(adder.count()).To(Equal(3))
	})

	It("mines a rotating slab of the embedded band when no URL is set", func() {
		w := New(baseCfg(), adder)
		w.perRun = 5

		w.tick(context.Background())
		first := append([]string(nil), adder.added...)
		Expect(first).To(HaveLen(5))

		adder.added = nil
		w.tick(context.Background())
		second := adder.added
		Expect(second).To(HaveLen(5))
		// Rotation: the cursor advanced, so the second slab differs from the first.
		Expect(second).NotTo(Equal(first))
	})

	It("logs but does not add when the fetch fails", func() {
		cfg := baseCfg()
		cfg.PrewarmURL = "http://trending.example/list"
		w := New(cfg, adder)
		w.get = func(_ context.Context, _ string) ([]byte, error) {
			return nil, fmt.Errorf("boom")
		}

		w.tick(context.Background())

		Expect(adder.count()).To(BeZero())
	})

	It("stops the interval loop on ctx cancel", func() {
		cfg := baseCfg()
		cfg.PrewarmURL = "http://trending.example/list"
		w := New(cfg, adder)
		w.interval = time.Millisecond
		w.get = func(_ context.Context, _ string) ([]byte, error) {
			return []byte("x.example\n"), nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			w.Run(ctx)
			close(done)
		}()

		Eventually(adder.count).Should(BeNumerically(">", 0)) // startup warm + ticks
		cancel()
		Eventually(done, "2s").Should(BeClosed())
	})
})
