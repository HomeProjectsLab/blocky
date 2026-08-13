package resolver

import (
	"context"
	"fmt"

	"github.com/0xERR0R/blocky/config"
	. "github.com/0xERR0R/blocky/helpertest"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DomainShardResolver", Label("domainShardResolver"), func() {
	var (
		sut       *DomainShardResolver
		upstreams []config.Upstream
		mocks     []*MockUDPUpstreamServer

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
			Strategy: config.UpstreamStrategyDomainShard,
			Timeout:  config.Duration(timeout),
		}

		sutConfig := config.NewUpstreamGroup("test", upstreamsCfg, upstreams)

		sut, err = NewDomainShardResolver(ctx, sutConfig, systemResolverBootstrap)
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

		It("should contain correct resolver in Name", func() {
			Expect(sut.Name()).Should(ContainSubstring(domainShardResolverType))
		})
	})

	Describe("shardKey", func() {
		It("reduces subdomains to the same eTLD+1", func() {
			Expect(shardKey("a.example.com.")).Should(Equal("example.com"))
			Expect(shardKey("b.sub.example.com.")).Should(Equal("example.com"))
			Expect(shardKey("EXAMPLE.com.")).Should(Equal("example.com"))
		})

		It("falls back to the full name when no eTLD+1 exists", func() {
			Expect(shardKey("com.")).Should(Equal("com"))
		})
	})

	Describe("sharding", func() {
		BeforeEach(func() {
			startMocks(2)
		})

		It("sends all subdomains of one eTLD+1 to the same upstream", func() {
			for _, domain := range []string{"a.example.com.", "b.example.com.", "x.y.example.com."} {
				_, err := sut.Resolve(ctx, newRequest(domain, A))
				Expect(err).ToNot(HaveOccurred())
			}

			counts := []int{mocks[0].GetCallCount(), mocks[1].GetCallCount()}
			Expect(counts).Should(ContainElement(3))
			Expect(counts).Should(ContainElement(0))
		})

		It("spreads different eTLD+1s across upstreams", func() {
			for i := range 10 {
				_, err := sut.Resolve(ctx, newRequest(fmt.Sprintf("domain%d.com.", i), A))
				Expect(err).ToNot(HaveOccurred())
			}

			Expect(mocks[0].GetCallCount()).Should(BeNumerically(">", 0))
			Expect(mocks[1].GetCallCount()).Should(BeNumerically(">", 0))
		})
	})

	Describe("salt rotation", func() {
		domains := func() []string {
			out := make([]string, 200)
			for i := range out {
				out[i] = fmt.Sprintf("domain%d.com", i)
			}

			return out
		}

		It("maps a domain consistently within a period (same salt)", func() {
			for _, d := range domains() {
				Expect(shardIndex(42, d, 4)).Should(Equal(shardIndex(42, d, 4)))
			}
		})

		It("moves some domains when the salt changes", func() {
			moved := 0

			for _, d := range domains() {
				if shardIndex(1, d, 4) != shardIndex(2, d, 4) {
					moved++
				}
			}

			// most domains should land elsewhere after a rotation
			Expect(moved).Should(BeNumerically(">", len(domains())/2))
		})

		It("keeps the N-upstream distribution balanced after rotation", func() {
			const n = 4

			for _, salt := range []uint64{0, 1, 7, 99} {
				counts := make([]int, n)
				for _, d := range domains() {
					counts[shardIndex(salt, d, n)]++
				}

				for _, c := range counts {
					Expect(c).Should(BeNumerically(">", 0))
				}
			}
		})
	})

	Describe("failover", func() {
		BeforeEach(func() {
			startMocks(1)
			upstreams = append([]config.Upstream{{Host: "wrong"}}, upstreams...)
		})

		It("walks the ring to the next upstream on failure", func() {
			for i := range 10 {
				_, err := sut.Resolve(ctx, newRequest(fmt.Sprintf("domain%d.com.", i), A))
				Expect(err).ToNot(HaveOccurred())
			}

			Expect(mocks[0].GetCallCount()).Should(Equal(10))
		})
	})
})
