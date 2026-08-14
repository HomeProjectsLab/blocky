package resolver

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/0xERR0R/blocky/config"
	. "github.com/0xERR0R/blocky/helpertest"
	"github.com/0xERR0R/blocky/log"
	. "github.com/0xERR0R/blocky/model"
	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("UpstreamTreeResolver", Label("upstreamTreeResolver"), func() {
	var (
		sut       Resolver
		sutConfig config.Upstreams

		err error

		ctx      context.Context
		cancelFn context.CancelFunc
	)

	BeforeEach(func() {
		ctx, cancelFn = context.WithCancel(context.Background())
		DeferCleanup(cancelFn)

		sutConfig = defaultUpstreamsConfig
	})

	JustBeforeEach(func() {
		sut, err = NewUpstreamTreeResolver(ctx, sutConfig, systemResolverBootstrap)
	})

	When("it has no configuration", func() {
		BeforeEach(func() {
			sutConfig = config.Upstreams{}
		})

		It("should return error", func() {
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(ContainSubstring("no external DNS resolvers configured")))
			Expect(sut).To(BeNil())
		})
	})

	When("it has only default group", func() {
		BeforeEach(func() {
			sutConfig.Groups = config.UpstreamGroups{
				upstreamDefaultCfgName: {
					{Host: "wrong"},
					{Host: "127.0.0.1"},
				},
			}
		})

		When("strategy is parallel", func() {
			BeforeEach(func() {
				sutConfig.Strategy = config.UpstreamStrategyParallelBest
			})

			It("keeps the tree with a single parallel_best branch", func() {
				Expect(err).ToNot(HaveOccurred())

				tree, ok := sut.(*UpstreamTreeResolver)
				Expect(ok).Should(BeTrue())
				Expect(tree.branches).Should(HaveLen(1))

				_, ok = tree.branches[upstreamDefaultCfgName].(*ParallelBestResolver)
				Expect(ok).Should(BeTrue())
			})
		})

		When("strategy is strict", func() {
			BeforeEach(func() {
				sutConfig.Strategy = config.UpstreamStrategyStrict
			})

			It("keeps the tree with a single strict branch", func() {
				Expect(err).ToNot(HaveOccurred())

				tree, ok := sut.(*UpstreamTreeResolver)
				Expect(ok).Should(BeTrue())
				Expect(tree.branches).Should(HaveLen(1))

				_, ok = tree.branches[upstreamDefaultCfgName].(*StrictResolver)
				Expect(ok).Should(BeTrue())
			})
		})

		When("strategy is recursive", func() {
			BeforeEach(func() {
				sutConfig.Strategy = config.UpstreamStrategyRecursive
			})

			It("creates a recursive resolver branch", func() {
				Expect(err).Should(Succeed())

				tree, ok := sut.(*UpstreamTreeResolver)
				Expect(ok).Should(BeTrue())
				Expect(tree.branches).Should(HaveLen(1))

				_, ok = tree.branches[upstreamDefaultCfgName].(*RecursiveResolver)
				Expect(ok).Should(BeTrue())
			})
		})

		When("a group overrides the strategy", func() {
			BeforeEach(func() {
				sutConfig.Strategy = config.UpstreamStrategyStrict
				sutConfig.GroupConfig = map[string]config.UpstreamGroupConfig{
					upstreamDefaultCfgName: {Strategy: config.UpstreamStrategyRoundRobin},
				}
			})

			It("uses the group's strategy for the branch", func() {
				Expect(err).ToNot(HaveOccurred())

				tree, ok := sut.(*UpstreamTreeResolver)
				Expect(ok).Should(BeTrue())

				_, ok = tree.branches[upstreamDefaultCfgName].(*RoundRobinResolver)
				Expect(ok).Should(BeTrue())
			})
		})
	})

	When("it has multiple groups", func() {
		BeforeEach(func() {
			sutConfig.Groups = config.UpstreamGroups{
				upstreamDefaultCfgName: {
					{Host: "wrong"},
					{Host: "127.0.0.1"},
				},
				"test": {
					{Host: "some-resolver"},
				},
			}
		})

		Describe("Type", func() {
			It("does not return error", func() {
				Expect(err).ToNot(HaveOccurred())
			})
			It("follows conventions", func() {
				expectValidResolverType(sut)
			})
			It("returns upstream_tree", func() {
				Expect(sut.Type()).To(Equal(upstreamTreeResolverType))
			})
		})

		Describe("Configuration output", func() {
			It("should return configuration", func() {
				Expect(sut.IsEnabled()).Should(BeTrue())

				logger, hook := log.NewMockEntry()
				sut.LogConfig(logger)
				Expect(hook.Calls).ToNot(BeEmpty())
			})
		})

		Describe("Name", func() {
			var utrSut *UpstreamTreeResolver
			JustBeforeEach(func() {
				utrSut = sut.(*UpstreamTreeResolver)
			})

			It("should contain correct resolver", func() {
				name := utrSut.Name()
				Expect(name).ShouldNot(BeEmpty())
				Expect(name).Should(ContainSubstring(upstreamTreeResolverType))
			})
		})

		When("init strategy is failOnError", func() {
			BeforeEach(func() {
				sutConfig.Init.Strategy = config.InitStrategyFailOnError
			})

			It("should fail", func() {
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("no valid upstream")))
				Expect(sut).To(BeNil())
			})
		})

		When("client specific resolvers are defined", func() {
			groups := map[string]string{
				upstreamDefaultCfgName: "127.0.0.1",
				"laptop":               "127.0.0.2",
				"client-*-m":           "127.0.0.3",
				"client[0-9]":          "127.0.0.4",
				"192.168.178.33":       "127.0.0.5",
				"10.43.8.67/28":        "127.0.0.6",
				"name-matches1":        "127.0.0.7",
				"name-matches*":        "127.0.0.8",
			}

			BeforeEach(func() {
				sutConfig.Groups = make(config.UpstreamGroups, len(groups))

				for group, ip := range groups {
					Expect(ip).ShouldNot(BeNil())

					server := NewMockUDPUpstreamServer().WithAnswerRR("example.com 123 IN A " + ip)
					sutConfig.Groups[group] = []config.Upstream{server.Start()}
				}
			})

			It("Should use default if client name or IP don't match", func() {
				request := newRequestWithClient("example.com.", A, "192.168.178.55", "test")

				Expect(sut.Resolve(ctx, request)).
					Should(
						SatisfyAll(
							BeDNSRecord("example.com.", A, groups["default"]),
							HaveResponseType(ResponseTypeRESOLVED),
							HaveReturnCode(dns.RcodeSuccess),
						))
			})
			It("Should use client specific resolver if client name matches exact", func() {
				request := newRequestWithClient("example.com.", A, "192.168.178.55", "laptop")

				Expect(sut.Resolve(ctx, request)).
					Should(
						SatisfyAll(
							BeDNSRecord("example.com.", A, groups["laptop"]),
							HaveResponseType(ResponseTypeRESOLVED),
							HaveReturnCode(dns.RcodeSuccess),
						))
			})
			It("Should use client specific resolver if client name matches with wildcard", func() {
				request := newRequestWithClient("example.com.", A, "192.168.178.55", "client-test-m")

				Expect(sut.Resolve(ctx, request)).
					Should(
						SatisfyAll(
							BeDNSRecord("example.com.", A, groups["client-*-m"]),
							HaveResponseType(ResponseTypeRESOLVED),
							HaveReturnCode(dns.RcodeSuccess),
						))
			})
			It("Should use client specific resolver if client name matches with range wildcard", func() {
				request := newRequestWithClient("example.com.", A, "192.168.178.55", "client7")

				Expect(sut.Resolve(ctx, request)).
					Should(
						SatisfyAll(
							BeDNSRecord("example.com.", A, groups["client[0-9]"]),
							HaveResponseType(ResponseTypeRESOLVED),
							HaveReturnCode(dns.RcodeSuccess),
						))
			})
			It("Should use client specific resolver if client IP matches", func() {
				request := newRequestWithClient("example.com.", A, "192.168.178.33", "noname")

				Expect(sut.Resolve(ctx, request)).
					Should(
						SatisfyAll(
							BeDNSRecord("example.com.", A, groups["192.168.178.33"]),
							HaveResponseType(ResponseTypeRESOLVED),
							HaveReturnCode(dns.RcodeSuccess),
						))
			})
			It("Should use client specific resolver if client name (containing IP) matches", func() {
				request := newRequestWithClient("example.com.", A, "0.0.0.0", "192.168.178.33")

				Expect(sut.Resolve(ctx, request)).
					Should(
						SatisfyAll(
							BeDNSRecord("example.com.", A, groups["192.168.178.33"]),
							HaveResponseType(ResponseTypeRESOLVED),
							HaveReturnCode(dns.RcodeSuccess),
						))
			})
			It("Should use client specific resolver if client's CIDR (10.43.8.64 - 10.43.8.79) matches", func() {
				request := newRequestWithClient("example.com.", A, "10.43.8.70", "noname")

				Expect(sut.Resolve(ctx, request)).
					Should(
						SatisfyAll(
							BeDNSRecord("example.com.", A, groups["10.43.8.67/28"]),
							HaveResponseType(ResponseTypeRESOLVED),
							HaveReturnCode(dns.RcodeSuccess),
						))
			})
			It("Should use exact IP match before client name match", func() {
				request := newRequestWithClient("example.com.", A, "192.168.178.33", "laptop")

				Expect(sut.Resolve(ctx, request)).
					Should(
						SatisfyAll(
							BeDNSRecord("example.com.", A, groups["192.168.178.33"]),
							HaveResponseType(ResponseTypeRESOLVED),
							HaveReturnCode(dns.RcodeSuccess),
						))
			})
			It("Should use client name match before CIDR match", func() {
				request := newRequestWithClient("example.com.", A, "10.43.8.70", "laptop")

				Expect(sut.Resolve(ctx, request)).
					Should(
						SatisfyAll(
							BeDNSRecord("example.com.", A, groups["laptop"]),
							HaveResponseType(ResponseTypeRESOLVED),
							HaveReturnCode(dns.RcodeSuccess),
						))
			})
			It("Should use one of the matching resolvers & log warning", func() {
				logger, hook := log.NewMockEntry()

				ctx, _ = log.NewCtx(ctx, logger)

				Expect(sut.Resolve(ctx, newRequestWithClient("example.com.", A, "0.0.0.0", "name-matches1"))).
					Should(
						SatisfyAll(
							SatisfyAny(
								BeDNSRecord("example.com.", A, groups["name-matches1"]),
								BeDNSRecord("example.com.", A, groups["name-matches*"]),
							),
							HaveResponseType(ResponseTypeRESOLVED),
							HaveReturnCode(dns.RcodeSuccess),
						))

				Expect(hook.Messages).Should(ContainElement(ContainSubstring("client matches multiple groups")))
			})
		})
	})

	Describe("ReplaceUpstreams", func() {
		var mockA, mockB *MockUDPUpstreamServer

		BeforeEach(func() {
			mockA = NewMockUDPUpstreamServer().WithAnswerRR("example.com 123 IN A 127.0.0.11")
			mockB = NewMockUDPUpstreamServer().WithAnswerRR("example.com 123 IN A 127.0.0.22")

			sutConfig = config.Upstreams{
				Init:    config.Init{Strategy: config.InitStrategyBlocking},
				Timeout: config.Duration(timeout),
				Groups: config.UpstreamGroups{
					upstreamDefaultCfgName: {mockA.Start()},
				},
			}
		})

		It("swaps the group's upstreams under concurrent queries", func() {
			Expect(err).ToNot(HaveOccurred())
			tree := sut.(*UpstreamTreeResolver)

			By("queries hit the old upstream", func() {
				Expect(sut.Resolve(ctx, newRequestWithClient("example.com.", A, "192.168.1.1"))).
					Should(BeDNSRecord("example.com.", A, "127.0.0.11"))
			})

			var (
				wg          sync.WaitGroup
				resolveErrs atomic.Int32
			)

			stop := make(chan struct{})

			By("swapping while queries are in flight", func() {
				for range 4 {

					wg.Go(func() {
						defer GinkgoRecover()

						for {
							select {
							case <-stop:
								return
							default:
							}

							_, err := tree.Resolve(ctx, newRequestWithClient("example.com.", A, "192.168.1.1"))
							if err != nil {
								resolveErrs.Add(1)
							}
						}
					})
				}

				Expect(tree.ReplaceUpstreams(ctx, upstreamDefaultCfgName, []config.Upstream{mockB.Start()})).
					Should(Succeed())

				close(stop)
				wg.Wait()

				Expect(resolveErrs.Load()).Should(BeZero())
			})

			By("subsequent queries hit the new upstream", func() {
				mockB.ResetCallCount()

				Expect(sut.Resolve(ctx, newRequestWithClient("example.com.", A, "192.168.1.1"))).
					Should(BeDNSRecord("example.com.", A, "127.0.0.22"))
				Expect(mockB.GetCallCount()).Should(BeNumerically(">", 0))
			})
		})

		It("errors for an unknown group", func() {
			Expect(err).ToNot(HaveOccurred())
			tree := sut.(*UpstreamTreeResolver)

			swapErr := tree.ReplaceUpstreams(ctx, "no-such-group", []config.Upstream{{Host: "127.0.0.1"}})
			Expect(swapErr).To(MatchError(ContainSubstring("unknown upstream group")))
		})
	})
})
