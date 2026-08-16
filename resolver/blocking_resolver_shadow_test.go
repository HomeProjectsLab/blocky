package resolver

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/0xERR0R/blocky/config"
	. "github.com/0xERR0R/blocky/helpertest"
	. "github.com/0xERR0R/blocky/model"

	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
)

// Shadow-blocked queries decouple the client answer from the wire query: a
// blocked domain still returns the block response to the client, but the real
// query is also egressed to the next resolver (answer discarded) so blocked
// trackers stay present in the real page-load cohort on the wire.
var _ = Describe("BlockingResolver shadow-blocked queries", func() {
	var (
		sut      *BlockingResolver
		m        *mockResolver
		ctx      context.Context
		cancelFn context.CancelFunc

		mu       sync.Mutex
		shadowed []string // question names the next resolver received
	)

	newSUT := func(shadow bool) {
		cfg := config.Blocking{
			BlockType: "ZEROIP",
			BlockTTL:  config.Duration(time.Minute),
			Denylists: map[string][]config.BytesSource{
				"ads": config.NewBytesSources(group1File.Path), // contains DOMAIN1.com
			},
			ClientGroupsBlock:    map[string][]string{"default": {"ads"}},
			ShadowBlockedQueries: shadow,
		}

		var err error
		sut, err = NewBlockingResolver(ctx, cfg, systemResolverBootstrap)
		Expect(err).Should(Succeed())

		m = &mockResolver{}
		m.On("Resolve", mock.Anything).Return(&Response{Res: new(dns.Msg)}, nil)
		m.ResolveFn = func(_ context.Context, req *Request) (*Response, error) {
			mu.Lock()
			shadowed = append(shadowed, strings.ToLower(req.Req.Question[0].Name))
			mu.Unlock()

			return &Response{Res: new(dns.Msg)}, nil
		}
		sut.Next(m)
	}

	receivedNames := func() []string {
		mu.Lock()
		defer mu.Unlock()

		return append([]string(nil), shadowed...)
	}

	BeforeEach(func() {
		ctx, cancelFn = context.WithCancel(context.Background())
		DeferCleanup(cancelFn)

		mu.Lock()
		shadowed = nil
		mu.Unlock()
	})

	It("returns the block response AND shadows the real query when enabled", func() {
		newSUT(true)

		Expect(sut.Resolve(ctx, newRequestWithClient("domain1.com.", A, "192.168.0.1", "client1"))).
			Should(HaveResponseType(ResponseTypeBLOCKED))

		Eventually(receivedNames).Should(ContainElement("domain1.com."))
	})

	It("does not shadow when disabled", func() {
		newSUT(false)

		Expect(sut.Resolve(ctx, newRequestWithClient("domain1.com.", A, "192.168.0.1", "client1"))).
			Should(HaveResponseType(ResponseTypeBLOCKED))

		Consistently(receivedNames, "300ms", "50ms").Should(BeEmpty())
	})

	It("never shadows a decoy query", func() {
		newSUT(true)

		req := newRequestWithClient("domain1.com.", A, "192.168.0.1", "client1")
		req.Decoy = true

		// A decoy bypasses blocking entirely (passthrough to next). That single
		// passthrough is the decoy's own egress, not a shadow — assert next is hit
		// exactly once and no extra shadow ever arrives.
		_, err := sut.Resolve(ctx, req)
		Expect(err).Should(Succeed())

		Consistently(func() int { return len(receivedNames()) }, "300ms", "50ms").Should(Equal(1))
	})

	It("drops the shadow (never queues) when too many are already in flight", func() {
		newSUT(true)

		// Fill the semaphore so the next shadow has no slot.
		for range cap(sut.shadowSem) {
			sut.shadowSem <- struct{}{}
		}

		Expect(sut.Resolve(ctx, newRequestWithClient("domain1.com.", A, "192.168.0.1", "client1"))).
			Should(HaveResponseType(ResponseTypeBLOCKED))

		Consistently(receivedNames, "300ms", "50ms").Should(BeEmpty())
	})

	It("does not shadow the same name twice within the suppress window", func() {
		newSUT(true)

		for range 3 {
			Expect(sut.Resolve(ctx, newRequestWithClient("domain1.com.", A, "192.168.0.1", "client1"))).
				Should(HaveResponseType(ResponseTypeBLOCKED))
		}

		Eventually(func() int { return len(receivedNames()) }).Should(Equal(1))
		Consistently(func() int { return len(receivedNames()) }, "300ms", "50ms").Should(Equal(1))
	})
})
