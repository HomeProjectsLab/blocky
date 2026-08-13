package resolver

import (
	"context"
	"fmt"

	"github.com/0xERR0R/blocky/config"
	. "github.com/0xERR0R/blocky/helpertest"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("RoundRobinResolver", Label("roundRobinResolver"), func() {
	var (
		sut         *RoundRobinResolver
		sutStrategy config.UpstreamStrategy
		upstreams   []config.Upstream
		mocks       []*MockUDPUpstreamServer

		err error

		ctx      context.Context
		cancelFn context.CancelFunc
	)

	// startMocks starts n mock upstreams answering with 127.0.0.<i+1>
	startMocks := func(n int) {
		mocks = make([]*MockUDPUpstreamServer, n)
		upstreams = make([]config.Upstream, n)

		for i := range n {
			mocks[i] = NewMockUDPUpstreamServer().
				WithAnswerRR(fmt.Sprintf("example.com 123 IN A 127.0.0.%d", i+1))
			upstreams[i] = mocks[i].Start()
		}
	}

	BeforeEach(func() {
		ctx, cancelFn = context.WithCancel(context.Background())
		DeferCleanup(cancelFn)

		sutStrategy = config.UpstreamStrategyRoundRobin
		mocks = nil
		upstreams = nil
	})

	JustBeforeEach(func() {
		upstreamsCfg := config.Upstreams{
			Init:     config.Init{Strategy: config.InitStrategyBlocking},
			Strategy: sutStrategy,
			Timeout:  config.Duration(timeout),
		}

		sutConfig := config.NewUpstreamGroup("test", upstreamsCfg, upstreams)

		sut, err = NewRoundRobinResolver(ctx, sutConfig, systemResolverBootstrap)
		Expect(err).ToNot(HaveOccurred())

		// drop the init test queries from the counts
		for _, m := range mocks {
			m.ResetCallCount()
		}
	})

	Describe("Type", func() {
		BeforeEach(func() {
			startMocks(1)
		})

		It("follows conventions", func() {
			expectValidResolverType(sut)
		})

		It("returns round_robin", func() {
			Expect(sut.Type()).Should(Equal(roundRobinResolverType))
		})
	})

	Describe("Name", func() {
		BeforeEach(func() {
			startMocks(1)
		})

		It("should contain correct resolver", func() {
			Expect(sut.Name()).Should(ContainSubstring(roundRobinResolverType))
		})
	})

	Describe("round_robin distribution", func() {
		BeforeEach(func() {
			startMocks(3)
		})

		It("cycles through all upstreams in order", func() {
			for i := range 6 {
				request := newRequest("example.com.", A)

				Expect(sut.Resolve(ctx, request)).
					Should(BeDNSRecord("example.com.", A, fmt.Sprintf("127.0.0.%d", i%3+1)))
			}

			for _, m := range mocks {
				Expect(m.GetCallCount()).Should(Equal(2))
			}
		})
	})

	Describe("weighted_round_robin distribution", func() {
		BeforeEach(func() {
			sutStrategy = config.UpstreamStrategyWeightedRoundRobin

			startMocks(2)
			upstreams[1].Weight = 2
		})

		It("returns weighted_round_robin as type", func() {
			Expect(sut.Type()).Should(Equal(weightedRoundRobinResolverType))
		})

		It("distributes queries proportionally to the weights", func() {
			for range 6 {
				request := newRequest("example.com.", A)

				_, err := sut.Resolve(ctx, request)
				Expect(err).ToNot(HaveOccurred())
			}

			Expect(mocks[0].GetCallCount()).Should(Equal(2))
			Expect(mocks[1].GetCallCount()).Should(Equal(4))
		})
	})

	Describe("failover", func() {
		BeforeEach(func() {
			startMocks(2)
			upstreams = append([]config.Upstream{{Host: "wrong"}}, upstreams...)
		})

		It("still answers every query when one upstream is dead", func() {
			for range 6 {
				request := newRequest("example.com.", A)

				_, err := sut.Resolve(ctx, request)
				Expect(err).ToNot(HaveOccurred())
			}

			Expect(mocks[0].GetCallCount() + mocks[1].GetCallCount()).Should(Equal(6))
		})
	})
})
