package lists

import (
	"context"
	"sync"

	"github.com/0xERR0R/blocky/config"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// countingProvider is an in-memory BlocklistProvider that records how many times
// each category is streamed, so a test can prove an unchanged group is NOT
// re-streamed on the second build. versions is folded into the fingerprint.
type countingProvider struct {
	mu       sync.Mutex
	domains  map[string][]string
	versions map[string]string
	streamed map[string]int
}

func newCountingProvider(domains map[string][]string) *countingProvider {
	return &countingProvider{
		domains:  domains,
		versions: map[string]string{},
		streamed: map[string]int{},
	}
}

func (p *countingProvider) ForEachBlocklistDomain(category string, fn func(domain string) error) error {
	p.mu.Lock()
	p.streamed[category]++
	ds := p.domains[category]
	p.mu.Unlock()

	for _, d := range ds {
		if err := fn(d); err != nil {
			return err
		}
	}

	return nil
}

func (p *countingProvider) BlocklistVersion(category string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.versions[category], nil
}

func (p *countingProvider) count(category string) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.streamed[category]
}

func (p *countingProvider) setVersion(category, v string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.versions[category] = v
}

var _ = Describe("Per-group cache reuse", func() {
	var (
		ctx      context.Context
		cancelFn context.CancelFunc
		cfg      config.SourceLoading
		provider *countingProvider
	)

	// build constructs a denylist ListCache from the given group->sources map and
	// blocks until the initial synchronous load finishes (blocking strategy).
	build := func(groups map[string][]config.BytesSource) *ListCache {
		c, err := NewListCache(ctx, ListCacheTypeDenylist, cfg, groups, nil)
		Expect(err).Should(Succeed())

		return c
	}

	blocklist := func(cat string) []config.BytesSource {
		return []config.BytesSource{{Type: config.BytesSourceTypeFile, From: BlocklistSourcePrefix + cat}}
	}

	BeforeEach(func() {
		ctx, cancelFn = context.WithCancel(context.Background())
		DeferCleanup(cancelFn)

		// Isolate the process-scoped reuse registry per spec.
		groupReuseMu.Lock()
		groupReuse = map[ListCacheType]map[string]reusableGroup{}
		groupReuseMu.Unlock()

		var err error
		cfg, err = config.WithDefaults[config.SourceLoading]()
		Expect(err).Should(Succeed())
		cfg.RefreshPeriod = -1 // no periodic goroutine; drive refresh explicitly

		provider = newCountingProvider(map[string][]string{
			"ads":     {"ads1.example.com", "ads2.example.com"},
			"malware": {"mal1.example.com"},
		})
		SetBlocklistProvider(provider)
	})

	AfterEach(func() {
		SetBlocklistProvider(nil)
	})

	It("reuses every group when nothing changed (no re-stream)", func() {
		groups := map[string][]config.BytesSource{
			"ads":     blocklist("ads"),
			"malware": blocklist("malware"),
		}

		build(groups)
		Expect(provider.count("ads")).Should(Equal(1))
		Expect(provider.count("malware")).Should(Equal(1))

		// Second build with byte-identical sources: both groups reused by reference.
		sut := build(groups)
		Expect(provider.count("ads")).Should(Equal(1), "ads must not be re-streamed")
		Expect(provider.count("malware")).Should(Equal(1), "malware must not be re-streamed")

		// Reused content is still correct.
		Expect(sut.Match("ads1.example.com", []string{"ads"})).Should(HaveKey("ads"))
		Expect(sut.Match("mal1.example.com", []string{"malware"})).Should(HaveKey("malware"))
	})

	It("rebuilds only the group whose sources changed (manual edit)", func() {
		build(map[string][]config.BytesSource{
			"ads":     blocklist("ads"),
			"malware": blocklist("malware"),
		})

		// Add a manual inline deny to the "ads" group only (the common UI edit).
		sut := build(map[string][]config.BytesSource{
			"ads":     append(blocklist("ads"), config.TextBytesSource("manual.example.com")),
			"malware": blocklist("malware"),
		})

		Expect(provider.count("ads")).Should(Equal(2), "changed group must rebuild")
		Expect(provider.count("malware")).Should(Equal(1), "unchanged giant must be reused")

		// The rebuilt group carries both the streamed and the manual entry.
		Expect(sut.Match("ads1.example.com", []string{"ads"})).Should(HaveKey("ads"))
		Expect(sut.Match("manual.example.com", []string{"ads"})).Should(HaveKey("ads"))
	})

	It("busts only the category whose version bumped (updater refresh)", func() {
		groups := map[string][]config.BytesSource{
			"ads":     blocklist("ads"),
			"malware": blocklist("malware"),
		}

		build(groups)

		// Updater re-fetched "ads": new content + bumped stored version.
		provider.mu.Lock()
		provider.domains["ads"] = []string{"ads1.example.com", "ads3.example.com"}
		provider.mu.Unlock()
		provider.setVersion("ads", "v2")

		sut := build(groups)

		Expect(provider.count("ads")).Should(Equal(2), "version bump must bust ads")
		Expect(provider.count("malware")).Should(Equal(1), "malware version unchanged, reuse")

		Expect(sut.Match("ads3.example.com", []string{"ads"})).Should(HaveKey("ads"))
	})

	It("does not pin dropped groups in the registry", func() {
		build(map[string][]config.BytesSource{
			"ads":     blocklist("ads"),
			"malware": blocklist("malware"),
		})

		// Drop malware (category toggled off): rebuild with only ads.
		build(map[string][]config.BytesSource{"ads": blocklist("ads")})

		groupReuseMu.Lock()
		_, malwareStillRegistered := groupReuse[ListCacheTypeDenylist]["malware"]
		_, adsRegistered := groupReuse[ListCacheTypeDenylist]["ads"]
		groupReuseMu.Unlock()

		Expect(malwareStillRegistered).Should(BeFalse(), "dropped group must be released for GC")
		Expect(adsRegistered).Should(BeTrue())
	})
})
