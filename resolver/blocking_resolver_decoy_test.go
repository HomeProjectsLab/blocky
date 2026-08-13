package resolver

import (
	"context"
	"time"

	"github.com/0xERR0R/blocky/config"
	. "github.com/0xERR0R/blocky/helpertest"
	. "github.com/0xERR0R/blocky/model"

	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
)

// A decoy request must never be blocked: a blocked decoy never leaves the box
// and therefore adds no cover traffic (see BlockingResolver.Resolve).
var _ = Describe("BlockingResolver decoy bypass", func() {
	var (
		sut      *BlockingResolver
		m        *mockResolver
		ctx      context.Context
		cancelFn context.CancelFunc
	)

	BeforeEach(func() {
		ctx, cancelFn = context.WithCancel(context.Background())
		DeferCleanup(cancelFn)

		cfg := config.Blocking{
			BlockType: "ZEROIP",
			BlockTTL:  config.Duration(time.Minute),
			Denylists: map[string][]config.BytesSource{
				"ads": config.NewBytesSources(group1File.Path), // contains DOMAIN1.com
			},
			ClientGroupsBlock: map[string][]string{"default": {"ads"}},
		}

		var err error
		sut, err = NewBlockingResolver(ctx, cfg, systemResolverBootstrap)
		Expect(err).Should(Succeed())

		m = &mockResolver{}
		m.On("Resolve", mock.Anything).Return(&Response{Res: new(dns.Msg)}, nil)
		sut.Next(m)
	})

	It("blocks a listed domain for a real query", func() {
		Expect(sut.Resolve(ctx, newRequestWithClient("domain1.com.", A, "192.168.0.1", "client1"))).
			Should(HaveResponseType(ResponseTypeBLOCKED))
		m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
	})

	It("passes the same listed domain through when the request is a decoy", func() {
		req := newRequestWithClient("domain1.com.", A, "192.168.0.1", "client1")
		req.Decoy = true

		resp, err := sut.Resolve(ctx, req)
		Expect(err).Should(Succeed())
		Expect(resp).ShouldNot(HaveResponseType(ResponseTypeBLOCKED))
		m.AssertCalled(GinkgoT(), "Resolve", mock.Anything)
	})
})
