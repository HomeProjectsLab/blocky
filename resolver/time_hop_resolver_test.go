package resolver

import (
	"context"
	"time"

	"github.com/0xERR0R/blocky/config"
	. "github.com/0xERR0R/blocky/helpertest"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("TimeHopResolver", Label("timeHopResolver"), func() {
	var (
		sut            *TimeHopResolver
		upstreams      []config.Upstream
		mocks          []*MockUDPUpstreamServer
		hopMin, hopMax time.Duration

		err error

		ctx      context.Context
		cancelFn context.CancelFunc
	)

	startMocks := func(n int) {
		mocks = make([]*MockUDPUpstreamServer, n)
		upstreams = make([]config.Upstream, n)

		for i := range n {
			mocks[i] = NewMockUDPUpstreamServer().WithAnswerRR("example.com 123 IN A 127.0.0.1")
			upstreams[i] = mocks[i].Start()
		}
	}

	BeforeEach(func() {
		ctx, cancelFn = context.WithCancel(context.Background())
		DeferCleanup(cancelFn)

		mocks = nil
		upstreams = nil
	})

	JustBeforeEach(func() {
		upstreamsCfg := config.Upstreams{
			Init:     config.Init{Strategy: config.InitStrategyBlocking},
			Strategy: config.UpstreamStrategyTimeHop,
			Timeout:  config.Duration(timeout),
			GroupConfig: map[string]config.UpstreamGroupConfig{
				"test": {
					Strategy: config.UpstreamStrategyTimeHop,
					HopMin:   config.Duration(hopMin),
					HopMax:   config.Duration(hopMax),
				},
			},
		}

		sutConfig := config.NewUpstreamGroup("test", upstreamsCfg, upstreams)

		sut, err = NewTimeHopResolver(ctx, sutConfig, systemResolverBootstrap)
		Expect(err).ToNot(HaveOccurred())

		// drop the init test queries from the counts
		for _, m := range mocks {
			m.ResetCallCount()
		}
	})

	Describe("Type", func() {
		BeforeEach(func() {
			hopMin, hopMax = 100*time.Millisecond, 200*time.Millisecond

			startMocks(1)
		})

		It("follows conventions", func() {
			expectValidResolverType(sut)
		})

		It("should contain correct resolver in Name", func() {
			Expect(sut.Name()).Should(ContainSubstring(timeHopResolverType))
		})
	})

	When("inside one hop window", func() {
		BeforeEach(func() {
			hopMin, hopMax = 400*time.Millisecond, 500*time.Millisecond

			startMocks(2)
		})

		It("sends all queries to a single upstream", func() {
			for range 10 {
				_, err := sut.Resolve(ctx, newRequest("example.com.", A))
				Expect(err).ToNot(HaveOccurred())
			}

			counts := []int{mocks[0].GetCallCount(), mocks[1].GetCallCount()}
			Expect(counts).Should(ContainElement(10))
			Expect(counts).Should(ContainElement(0))
		})
	})

	When("hop windows expire", func() {
		BeforeEach(func() {
			hopMin, hopMax = 30*time.Millisecond, 60*time.Millisecond

			startMocks(2)
		})

		It("hops across upstreams over time", func() {
			for range 15 {
				_, err := sut.Resolve(ctx, newRequest("example.com.", A))
				Expect(err).ToNot(HaveOccurred())

				time.Sleep(70 * time.Millisecond)
			}

			Expect(mocks[0].GetCallCount()).Should(BeNumerically(">", 0))
			Expect(mocks[1].GetCallCount()).Should(BeNumerically(">", 0))
		})
	})

	When("the current upstream fails", func() {
		BeforeEach(func() {
			hopMin, hopMax = 400*time.Millisecond, 500*time.Millisecond

			startMocks(1)
			upstreams = append([]config.Upstream{{Host: "wrong"}}, upstreams...)
		})

		It("hops to a different upstream and answers", func() {
			for range 5 {
				_, err := sut.Resolve(ctx, newRequest("example.com.", A))
				Expect(err).ToNot(HaveOccurred())
			}

			Expect(mocks[0].GetCallCount()).Should(BeNumerically(">=", 5))
		})
	})
})
