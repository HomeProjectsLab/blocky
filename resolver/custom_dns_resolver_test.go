package resolver

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/0xERR0R/blocky/config"
	. "github.com/0xERR0R/blocky/helpertest"
	"github.com/0xERR0R/blocky/log"
	. "github.com/0xERR0R/blocky/model"
	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
)

var _ = Describe("CustomDNSResolver", func() {
	var (
		TTL     = uint32(time.Now().Second())
		zoneTTL = uint32(time.Now().Second() * 2)

		sut *CustomDNSResolver
		m   *mockResolver
		cfg config.CustomDNS

		ctx      context.Context
		cancelFn context.CancelFunc
	)

	Describe("Type", func() {
		It("follows conventions", func() {
			expectValidResolverType(sut)
		})
	})

	BeforeEach(func() {
		ctx, cancelFn = context.WithCancel(context.Background())
		DeferCleanup(cancelFn)

		zoneHdr := dns.RR_Header{Ttl: zoneTTL}

		cfg = config.CustomDNS{
			Mapping: config.CustomDNSMapping{
				"custom.domain": {&dns.A{A: net.ParseIP("192.168.143.123")}},
				"ip6.domain":    {&dns.AAAA{AAAA: net.ParseIP("2001:0db8:85a3:0000:0000:8a2e:0370:7334")}},
				"multiple.ips": {
					&dns.A{A: net.ParseIP("192.168.143.123")},
					&dns.A{A: net.ParseIP("192.168.143.125")},
					&dns.AAAA{AAAA: net.ParseIP("2001:0db8:85a3:0000:0000:8a2e:0370:7334")},
				},
			},
			Zone: config.ZoneFileDNS{
				RRs: config.CustomDNSMapping{
					"example.zone.":    {&dns.A{A: net.ParseIP("1.2.3.4"), Hdr: zoneHdr}},
					"cname.domain.":    {&dns.CNAME{Target: "custom.domain", Hdr: zoneHdr}},
					"cname.ip6.":       {&dns.CNAME{Target: "ip6.domain", Hdr: zoneHdr}},
					"cname.example.":   {&dns.CNAME{Target: "example.com", Hdr: zoneHdr}},
					"cname.recursive.": {&dns.CNAME{Target: "cname.recursive", Hdr: zoneHdr}},
					"srv.":             {&dns.SRV{Priority: 0, Weight: 5, Port: 12345, Target: "service", Hdr: zoneHdr}},
					"txt.":             {&dns.TXT{Txt: []string{"space", "separated", "value"}, Hdr: zoneHdr}},
					"mx.domain.":       {&dns.MX{Mx: "mx.domain", Hdr: dns.RR_Header{Ttl: zoneTTL, Rrtype: dns.TypeMX}}},
				},
			},
			CustomTTL:           config.Duration(time.Duration(TTL) * time.Second),
			FilterUnmappedTypes: true,
		}
	})

	JustBeforeEach(func() {
		sut = NewCustomDNSResolver(cfg)
		m = &mockResolver{}
		m.On("Resolve", mock.Anything).Return(&Response{Res: new(dns.Msg)}, nil)
		sut.Next(m)
	})

	Describe("NXDOMAIN entries", func() {
		BeforeEach(func() {
			cfg.NXDomains = []string{"use-application-dns.net"}
		})

		It("answers a configured nxdomain with NXDOMAIN instead of a record", func() {
			Expect(sut.Resolve(ctx, newRequest("use-application-dns.net.", A))).
				Should(HaveReturnCode(dns.RcodeNameError))
		})

		It("does not affect domains that aren't listed", func() {
			Expect(sut.Resolve(ctx, newRequest("custom.domain.", A))).
				Should(HaveReturnCode(dns.RcodeSuccess))
		})
	})

	Describe("IsEnabled", func() {
		It("is true", func() {
			Expect(sut.IsEnabled()).Should(BeTrue())
		})
	})

	Describe("LogConfig", func() {
		It("should log something", func() {
			logger, hook := log.NewMockEntry()

			sut.LogConfig(logger)

			Expect(hook.Calls).ShouldNot(BeEmpty())
		})
	})

	Describe("Resolving custom name via CustomDNSResolver", func() {
		When("The parent context has an error ", func() {
			It("should return the error", func() {
				cancelledCtx, cancel := context.WithCancel(context.Background())
				cancel()

				_, err := sut.Resolve(cancelledCtx, newRequest("custom.domain.", A))

				Expect(err).Should(HaveOccurred())
				Expect(err.Error()).Should(ContainSubstring("context canceled"))
			})
		})
		When("Creating the IP response returns an error ", func() {
			It("should return the error", func() {
				createAnswerMock := func(_ dns.Question, _ net.IP, _ uint32) (dns.RR, error) {
					return nil, errors.New("create answer error")
				}

				sut.CreateAnswerFromQuestion(createAnswerMock)

				_, err := sut.Resolve(ctx, newRequest("custom.domain.", A))

				Expect(err).Should(HaveOccurred())
				Expect(err.Error()).Should(ContainSubstring("create answer error"))
			})
		})
		When("The forward request returns an error ", func() {
			It("should return the error if the error occurs when checking ipv4 forward addresses", func() {
				err := errors.New("forward error")
				m = &mockResolver{}

				m.On("Resolve", mock.Anything).Return(nil, err)

				sut.Next(m)
				_, err = sut.Resolve(ctx, newRequest("cname.example.", A))

				Expect(err).Should(HaveOccurred())
				Expect(err.Error()).Should(ContainSubstring("forward error"))
			})
			It("should return the error if the error occurs when checking ipv6 forward addresses", func() {
				err := errors.New("forward error")
				m = &mockResolver{}

				m.On("Resolve", mock.Anything).Return(nil, err)

				sut.Next(m)
				_, err = sut.Resolve(ctx, newRequest("cname.example.", AAAA))

				Expect(err).Should(HaveOccurred())
				Expect(err.Error()).Should(ContainSubstring("forward error"))
			})
		})
		When("Ip 4 mapping is defined for custom domain and", func() {
			Context("filterUnmappedTypes is true", func() {
				BeforeEach(func() { cfg.FilterUnmappedTypes = true })
				It("defined ip4 query should be resolved from zone mappings and should use the TTL defined in the zone", func() {
					Expect(sut.Resolve(ctx, newRequest("example.zone.", A))).
						Should(
							SatisfyAll(
								BeDNSRecord("example.zone.", A, "1.2.3.4"),
								HaveTTL(BeNumerically("==", zoneTTL)),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))
					// will not delegate to next resolver
					m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
				})
				It("defined ip4 query should be resolved", func() {
					Expect(sut.Resolve(ctx, newRequest("custom.domain.", A))).
						Should(
							SatisfyAll(
								BeDNSRecord("custom.domain.", A, "192.168.143.123"),
								HaveTTL(BeNumerically("==", TTL)),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))
					// will not delegate to next resolver
					m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
				})
				It("TXT query for defined mapping should return NOERROR and empty result", func() {
					Expect(sut.Resolve(ctx, newRequest("custom.domain.", TXT))).
						Should(
							SatisfyAll(
								HaveNoAnswer(),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))
					// will not delegate to next resolver
					m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
				})
				It("ip6 query should return NOERROR and empty result", func() {
					Expect(sut.Resolve(ctx, newRequest("custom.domain.", AAAA))).
						Should(
							SatisfyAll(
								HaveNoAnswer(),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))
					// will not delegate to next resolver
					m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
				})
			})

			Context("filterUnmappedTypes is false", func() {
				BeforeEach(func() { cfg.FilterUnmappedTypes = false })
				It("defined ip4 query should be resolved", func() {
					Expect(sut.Resolve(ctx, newRequest("custom.domain.", A))).
						Should(
							SatisfyAll(
								BeDNSRecord("custom.domain.", A, "192.168.143.123"),
								HaveTTL(BeNumerically("==", TTL)),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))
					// will not delegate to next resolver
					m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
				})
				It("TXT query for defined mapping should be delegated to next resolver", func() {
					Expect(sut.Resolve(ctx, newRequest("custom.domain.", TXT))).
						Should(
							SatisfyAll(
								HaveNoAnswer(),
								HaveResponseType(ResponseTypeRESOLVED),
								HaveReturnCode(dns.RcodeSuccess),
							))

					// delegate was executed
					m.AssertExpectations(GinkgoT())
				})
				It("ip6 query should return NOERROR and empty result", func() {
					Expect(sut.Resolve(ctx, newRequest("custom.domain.", AAAA))).
						Should(
							SatisfyAll(
								HaveNoAnswer(),
								HaveResponseType(ResponseTypeRESOLVED),
								HaveReturnCode(dns.RcodeSuccess),
							))

					// delegate was executed
					m.AssertExpectations(GinkgoT())
				})
			})
		})
		When("Ip 6 mapping is defined for custom domain ", func() {
			It("ip6 query should be resolved", func() {
				Expect(sut.Resolve(ctx, newRequest("ip6.domain.", AAAA))).
					Should(
						SatisfyAll(
							BeDNSRecord("ip6.domain.", AAAA, "2001:db8:85a3::8a2e:370:7334"),
							HaveTTL(BeNumerically("==", TTL)),
							HaveResponseType(ResponseTypeCUSTOMDNS),
							HaveReason("CUSTOM DNS"),
							HaveReturnCode(dns.RcodeSuccess),
						))
				// will not delegate to next resolver
				m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
			})
		})
		When("Multiple IPs are defined for custom domain ", func() {
			It("all IPs for the current type should be returned", func() {
				By("IPv6 query", func() {
					Expect(sut.Resolve(ctx, newRequest("multiple.ips.", AAAA))).
						Should(
							SatisfyAll(
								BeDNSRecord("multiple.ips.", AAAA, "2001:db8:85a3::8a2e:370:7334"),
								HaveTTL(BeNumerically("==", TTL)),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))

					// will not delegate to next resolver
					m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
				})

				By("IPv4 query", func() {
					Expect(sut.Resolve(ctx, newRequest("multiple.ips.", A))).
						Should(
							SatisfyAll(
								WithTransform(ToAnswer, SatisfyAll(
									HaveLen(2),
									ContainElements(
										BeDNSRecord("multiple.ips.", A, "192.168.143.123"),
										BeDNSRecord("multiple.ips.", A, "192.168.143.125")),
								)),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))

					// will not delegate to next resolver
					m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
				})
			})
		})
		When("A CNAME record is defined for custom domain ", func() {
			It("should not recurse if the request is strictly a CNAME request", func() {
				By("CNAME query", func() {
					Expect(sut.Resolve(ctx, newRequest("cname.domain", CNAME))).
						Should(
							SatisfyAll(
								WithTransform(ToAnswer, SatisfyAll(
									HaveLen(1),
									ContainElements(
										BeDNSRecord("cname.domain.", CNAME, "custom.domain.")),
								)),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))

					// will not delegate to next resolver
					m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
				})
			})
			It("all CNAMES for the current type should be recursively resolved when relying on other Mappings", func() {
				By("A query", func() {
					Expect(sut.Resolve(ctx, newRequest("cname.domain", A))).
						Should(
							SatisfyAll(
								WithTransform(ToAnswer, SatisfyAll(
									HaveLen(2),
									ContainElements(
										BeDNSRecord("cname.domain.", CNAME, "custom.domain."),
										BeDNSRecord("custom.domain.", A, "192.168.143.123")),
								)),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))

					// will not delegate to next resolver
					m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
				})
				By("AAAA query", func() {
					Expect(sut.Resolve(ctx, newRequest("cname.ip6", AAAA))).
						Should(
							SatisfyAll(
								WithTransform(ToAnswer, SatisfyAll(
									HaveLen(2),
									ContainElements(
										BeDNSRecord("cname.ip6.", CNAME, "ip6.domain."),
										BeDNSRecord("ip6.domain.", AAAA, "2001:db8:85a3::8a2e:370:7334")),
								)),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))

					// will not delegate to next resolver
					m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
				})
			})
			It("should return an error when the CNAME is recursive", func() {
				By("CNAME query", func() {
					_, err := sut.Resolve(ctx, newRequest("cname.recursive", A))
					Expect(err).Should(HaveOccurred())
					Expect(err.Error()).Should(ContainSubstring("CNAME loop detected:"))
					// will not delegate to next resolver
					m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
				})
			})
			It("all CNAMES for the current type should be returned when relying on public DNS", func() {
				By("CNAME query", func() {
					Expect(sut.Resolve(ctx, newRequest("cname.example", A))).
						Should(
							SatisfyAll(
								WithTransform(ToAnswer, SatisfyAll(
									ContainElements(
										BeDNSRecord("cname.example.", CNAME, "example.com.")),
								)),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))

					// will delegate to next resolver
					m.AssertCalled(GinkgoT(), "Resolve", mock.Anything)
				})
			})
			It("should not panic when CNAME points to external domain and next resolver returns NoResponse", func() {
				// This reproduces issue #1867: panic when CNAME points to external domain
				// When CustomDNSResolver is wrapped by RewriterResolver (as it is in server.go),
				// the next resolver is set to NoOpResolver, which returns NoResponse.
				// The bug occurs because processCNAME tries to access targetResp.Res.Answer
				// when targetResp is NoResponse (where Res is nil).

				// Set next resolver to NoOpResolver to simulate RewriterResolver wrapping
				sut.Next(NewNoOpResolver())

				// This should not panic, even though NoOpResolver returns NoResponse
				resp, err := sut.Resolve(ctx, newRequest("cname.example", A))

				// Should not panic or error
				Expect(err).ShouldNot(HaveOccurred())

				// Should return the CNAME record (but not the target A record since next resolver has no answer)
				Expect(resp).Should(
					SatisfyAll(
						WithTransform(ToAnswer, SatisfyAll(
							ContainElements(
								BeDNSRecord("cname.example.", CNAME, "example.com.")),
						)),
						HaveResponseType(ResponseTypeCUSTOMDNS),
						HaveReason("CUSTOM DNS"),
						HaveReturnCode(dns.RcodeSuccess),
					))
			})
		})
		When("Querying other record types", func() {
			It("Returns an SRV response", func() {
				Expect(sut.Resolve(ctx, newRequest("srv", SRV))).
					Should(
						SatisfyAll(
							WithTransform(ToAnswer, SatisfyAll(
								ContainElements(
									BeDNSRecord("srv.", SRV, "0 5 12345 service")),
							)),
							HaveResponseType(ResponseTypeCUSTOMDNS),
							HaveReason("CUSTOM DNS"),
							HaveReturnCode(dns.RcodeSuccess),
						))
			})
			It("Returns a TXT response", func() {
				Expect(sut.Resolve(ctx, newRequest("txt", TXT))).
					Should(
						SatisfyAll(
							WithTransform(ToAnswer, SatisfyAll(
								ContainElements(
									BeDNSRecord("txt.", TXT, "space separated value")),
							)),
							HaveResponseType(ResponseTypeCUSTOMDNS),
							HaveReason("CUSTOM DNS"),
							HaveReturnCode(dns.RcodeSuccess),
						))
			})
		})
		When("A generic RR type (e.g. MX) is queried and found in the config mapping ", func() {
			It("should be served generically", func() {
				By("MX query", func() {
					Expect(sut.Resolve(ctx, newRequest("mx.domain.", MX))).
						Should(
							SatisfyAll(
								WithTransform(ToAnswer, ContainElements(
									BeDNSRecord("mx.domain.", MX, "mx.domain"))),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))
				})
			})
		})
		When("Reverse DNS request is received", func() {
			It("should resolve the defined domain name", func() {
				By("ipv4", func() {
					Expect(sut.Resolve(ctx, newRequest("123.143.168.192.in-addr.arpa.", PTR))).
						Should(
							SatisfyAll(
								WithTransform(ToAnswer, SatisfyAll(
									HaveLen(2),
									ContainElements(
										BeDNSRecord("123.143.168.192.in-addr.arpa.", PTR, "custom.domain."),
										BeDNSRecord("123.143.168.192.in-addr.arpa.", PTR, "multiple.ips.")),
								)),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))

					// will not delegate to next resolver
					m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
				})

				By("ipv6", func() {
					Expect(sut.Resolve(ctx, newRequest("4.3.3.7.0.7.3.0.e.2.a.8.0.0.0.0.0.0.0.0.3.a.5.8.8.b.d.0.1.0.0.2.ip6.arpa.",
						PTR))).
						Should(
							SatisfyAll(
								WithTransform(ToAnswer, SatisfyAll(
									HaveLen(2),
									ContainElements(
										BeDNSRecord("4.3.3.7.0.7.3.0.e.2.a.8.0.0.0.0.0.0.0.0.3.a.5.8.8.b.d.0.1.0.0.2.ip6.arpa.",
											PTR, "ip6.domain."),
										BeDNSRecord("4.3.3.7.0.7.3.0.e.2.a.8.0.0.0.0.0.0.0.0.3.a.5.8.8.b.d.0.1.0.0.2.ip6.arpa.",
											PTR, "multiple.ips.")),
								)),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))

					// will not delegate to next resolver
					m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
				})
			})
		})
		When("Reverse DNS request uses a different case", func() {
			It("should resolve the defined domain name", func() {
				By("ipv4", func() {
					Expect(sut.Resolve(ctx, newRequest("123.143.168.192.IN-ADDR.ARPA.", PTR))).
						Should(
							SatisfyAll(
								WithTransform(ToAnswer, SatisfyAll(
									HaveLen(2),
									ContainElements(
										BeDNSRecord("123.143.168.192.IN-ADDR.ARPA.", PTR, "custom.domain."),
										BeDNSRecord("123.143.168.192.IN-ADDR.ARPA.", PTR, "multiple.ips.")),
								)),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))

					// will not delegate to next resolver
					m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
				})

				By("ipv6", func() {
					Expect(sut.Resolve(ctx, newRequest("4.3.3.7.0.7.3.0.E.2.A.8.0.0.0.0.0.0.0.0.3.A.5.8.8.B.D.0.1.0.0.2.IP6.ARPA.",
						PTR))).
						Should(
							SatisfyAll(
								WithTransform(ToAnswer, SatisfyAll(
									HaveLen(2),
									ContainElements(
										BeDNSRecord("4.3.3.7.0.7.3.0.E.2.A.8.0.0.0.0.0.0.0.0.3.A.5.8.8.B.D.0.1.0.0.2.IP6.ARPA.",
											PTR, "ip6.domain."),
										BeDNSRecord("4.3.3.7.0.7.3.0.E.2.A.8.0.0.0.0.0.0.0.0.3.A.5.8.8.B.D.0.1.0.0.2.IP6.ARPA.",
											PTR, "multiple.ips.")),
								)),
								HaveResponseType(ResponseTypeCUSTOMDNS),
								HaveReason("CUSTOM DNS"),
								HaveReturnCode(dns.RcodeSuccess),
							))

					// will not delegate to next resolver
					m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
				})
			})
		})
		When("An explicit PTR is configured for an IP that also has a forward A", func() {
			BeforeEach(func() {
				// forward A for 10.0.0.5 synthesizes reverse 5.0.0.10.in-addr.arpa -> host.lan
				cfg.Mapping["host.lan"] = config.CustomDNSEntries{&dns.A{A: net.ParseIP("10.0.0.5")}}
				// explicit PTR for the SAME arpa name, pointing elsewhere
				cfg.Zone.RRs["5.0.0.10.in-addr.arpa."] = config.CustomDNSEntries{
					&dns.PTR{Ptr: "other.lan.", Hdr: dns.RR_Header{Rrtype: dns.TypePTR, Ttl: zoneTTL}},
				}
			})

			It("serves the explicit PTR, not the synthesized reverse", func() {
				Expect(sut.Resolve(ctx, newRequest("5.0.0.10.in-addr.arpa.", PTR))).
					Should(
						SatisfyAll(
							WithTransform(ToAnswer, SatisfyAll(
								HaveLen(1),
								ContainElement(BeDNSRecord("5.0.0.10.in-addr.arpa.", PTR, "other.lan.")),
							)),
							HaveResponseType(ResponseTypeCUSTOMDNS),
							HaveReason("CUSTOM DNS"),
							HaveReturnCode(dns.RcodeSuccess),
						))
			})
		})
		When("Domain mapping is defined", func() {
			It("subdomain must also match", func() {
				Expect(sut.Resolve(ctx, newRequest("ABC.CUSTOM.DOMAIN.", A))).
					Should(
						SatisfyAll(
							BeDNSRecord("ABC.CUSTOM.DOMAIN.", A, "192.168.143.123"),
							HaveTTL(BeNumerically("==", TTL)),
							HaveResponseType(ResponseTypeCUSTOMDNS),
							HaveReason("CUSTOM DNS"),
							HaveReturnCode(dns.RcodeSuccess),
						))
				// will not delegate to next resolver
				m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
			})
		})
	})

	Describe("LookupReverse", func() {
		It("returns the mapped domain names for a known IPv4 address", func() {
			Expect(sut.LookupReverse(net.ParseIP("192.168.143.123"))).
				Should(ConsistOf("custom.domain", "multiple.ips"))
		})

		It("returns the mapped domain names for a known IPv6 address", func() {
			Expect(sut.LookupReverse(net.ParseIP("2001:0db8:85a3:0000:0000:8a2e:0370:7334"))).
				Should(ConsistOf("ip6.domain", "multiple.ips"))
		})

		It("returns nil for an unknown IP", func() {
			Expect(sut.LookupReverse(net.ParseIP("8.8.8.8"))).Should(BeNil())
		})
	})

	Describe("Delegating to next resolver", func() {
		When("no mapping for domain exist", func() {
			It("should delegate to next resolver", func() {
				Expect(sut.Resolve(ctx, newRequest("example.com.", A))).
					Should(
						SatisfyAll(
							HaveResponseType(ResponseTypeRESOLVED),
							HaveReturnCode(dns.RcodeSuccess),
						))

				// delegate was executed
				m.AssertExpectations(GinkgoT())
			})
		})
	})

	Describe("Domain rewriting", func() {
		BeforeEach(func() {
			cfg.Rewrite = map[string]string{
				"source.test": "custom.domain",
				"ip6.test":    "ip6.domain",
			}
			// Recreate resolver with rewrite configuration
			sut = NewCustomDNSResolver(cfg)
			m = &mockResolver{}
			m.On("Resolve", mock.Anything).Return(&Response{Res: new(dns.Msg)}, nil)
			sut.Next(m)
		})

		When("request matches rewrite rule", func() {
			It("should rewrite subdomain and resolve from mapping", func() {
				// Request for www.source.test should be rewritten to www.custom.domain
				// and resolved from the mapping (custom.domain matches)
				Expect(sut.Resolve(ctx, newRequest("www.source.test.", A))).
					Should(
						SatisfyAll(
							BeDNSRecord("www.source.test.", A, "192.168.143.123"),
							HaveTTL(BeNumerically("==", TTL)),
							HaveResponseType(ResponseTypeCUSTOMDNS),
							HaveReason("CUSTOM DNS"),
							HaveReturnCode(dns.RcodeSuccess),
						))

				// will not delegate to next resolver
				m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
			})

			It("should rewrite nested subdomain and resolve from mapping", func() {
				// Nested subdomain should also be rewritten
				Expect(sut.Resolve(ctx, newRequest("api.www.source.test.", A))).
					Should(
						SatisfyAll(
							BeDNSRecord("api.www.source.test.", A, "192.168.143.123"),
							HaveTTL(BeNumerically("==", TTL)),
							HaveResponseType(ResponseTypeCUSTOMDNS),
							HaveReason("CUSTOM DNS"),
							HaveReturnCode(dns.RcodeSuccess),
						))

				// will not delegate to next resolver
				m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
			})

			It("should rewrite to IPv6 mapping", func() {
				Expect(sut.Resolve(ctx, newRequest("www.ip6.test.", AAAA))).
					Should(
						SatisfyAll(
							BeDNSRecord("www.ip6.test.", AAAA, "2001:db8:85a3::8a2e:370:7334"),
							HaveTTL(BeNumerically("==", TTL)),
							HaveResponseType(ResponseTypeCUSTOMDNS),
							HaveReason("CUSTOM DNS"),
							HaveReturnCode(dns.RcodeSuccess),
						))

				// will not delegate to next resolver
				m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
			})

			It("should preserve original domain name in response", func() {
				resp, err := sut.Resolve(ctx, newRequest("www.source.test.", A))
				Expect(err).ShouldNot(HaveOccurred())

				// Question should have original name, not rewritten name
				Expect(resp.Res.Question[0].Name).Should(Equal("www.source.test."))
				// Answer should have original name, not rewritten name
				Expect(resp.Res.Answer[0].Header().Name).Should(Equal("www.source.test."))
			})
		})

		When("request does not match rewrite rule", func() {
			It("should not rewrite and handle normally", func() {
				Expect(sut.Resolve(ctx, newRequest("custom.domain.", A))).
					Should(
						SatisfyAll(
							BeDNSRecord("custom.domain.", A, "192.168.143.123"),
							HaveTTL(BeNumerically("==", TTL)),
							HaveResponseType(ResponseTypeCUSTOMDNS),
							HaveReason("CUSTOM DNS"),
							HaveReturnCode(dns.RcodeSuccess),
						))

				// will not delegate to next resolver
				m.AssertNotCalled(GinkgoT(), "Resolve", mock.Anything)
			})
		})
	})
})

// --- Fuzz + property tests for the customDNS answer path ---------------------
//
// Invariant under test: for a *present* mapped name, resolution never SERVFAILs
// and never panics regardless of the stored RR type vs. the queried qtype. A
// present-but-wrong type yields NODATA (NOERROR + empty answer), not an error.
// Property: queried type present -> answer returned; absent -> empty answer.

// fuzzRR returns a representative RR of the given DNS type, named on the mapped
// domain. CNAME is included for the no-panic/no-SERVFAIL invariant but is
// excluded from the present/absent property below (a CNAME is chased, so it is
// returned even for a differing qtype — standard DNS, not NODATA).
func fuzzRR(qtype uint16) dns.RR {
	hdr := dns.RR_Header{Name: "fuzz.example.", Class: dns.ClassINET, Rrtype: qtype, Ttl: 3600}

	switch qtype {
	case dns.TypeA:
		return &dns.A{Hdr: hdr, A: net.ParseIP("1.2.3.4")}
	case dns.TypeAAAA:
		return &dns.AAAA{Hdr: hdr, AAAA: net.ParseIP("2001:db8::1")}
	case dns.TypeTXT:
		return &dns.TXT{Hdr: hdr, Txt: []string{"fuzz", "value"}}
	case dns.TypeSRV:
		return &dns.SRV{Hdr: hdr, Priority: 1, Weight: 5, Port: 8080, Target: "svc.example."}
	case dns.TypeCNAME:
		return &dns.CNAME{Hdr: hdr, Target: "other.example."}
	case dns.TypeMX:
		return &dns.MX{Hdr: hdr, Preference: 10, Mx: "mail.example."}
	case dns.TypeNS:
		return &dns.NS{Hdr: hdr, Ns: "ns.example."}
	case dns.TypePTR:
		return &dns.PTR{Hdr: hdr, Ptr: "ptr.example."}
	case dns.TypeCAA:
		return &dns.CAA{Hdr: hdr, Flag: 0, Tag: "issue", Value: "ca.example"}
	default:
		return nil
	}
}

// fuzzTypes is the set of RR types both stored and queried by the fuzz/property
// tests. Keep A/AAAA/TXT/SRV/CNAME first so the special-cased answer paths are
// always exercised.
var fuzzTypes = []uint16{ //nolint:gochecknoglobals
	dns.TypeA, dns.TypeAAAA, dns.TypeTXT, dns.TypeSRV, dns.TypeCNAME,
	dns.TypeMX, dns.TypeNS, dns.TypePTR, dns.TypeCAA,
}

// checkCustomDNSAnswerPath asserts the invariants for one (storedType, queriedType)
// pair. It fails the test (via t) on any violation.
func checkCustomDNSAnswerPath(t *testing.T, storedType, queriedType uint16) {
	t.Helper()

	rr := fuzzRR(storedType)
	if rr == nil {
		return // type not in our representative set
	}

	sut := NewCustomDNSResolver(config.CustomDNS{
		Mapping:             config.CustomDNSMapping{"fuzz.example.": {rr}},
		FilterUnmappedTypes: true,
	})
	sut.Next(NewNoOpResolver())

	resp, err := sut.Resolve(context.Background(), newRequest("fuzz.example.", dns.Type(queriedType)))
	// Present name must never SERVFAIL.
	if err != nil {
		t.Fatalf("stored=%d queried=%d: unexpected error (SERVFAIL): %v", storedType, queriedType, err)
	}
	if resp == nil || resp.Res == nil {
		t.Fatalf("stored=%d queried=%d: nil response for present name", storedType, queriedType)
	}
	if resp.Res.Rcode == dns.RcodeServerFailure {
		t.Fatalf("stored=%d queried=%d: SERVFAIL rcode on present name", storedType, queriedType)
	}

	// present/absent property. CNAME is chased, so skip it as a stored type.
	if storedType == dns.TypeCNAME {
		return
	}

	got := len(resp.Res.Answer)
	if queriedType == storedType {
		if got == 0 {
			t.Fatalf("stored=%d queried=%d: type present but no answer returned", storedType, queriedType)
		}
	} else if got != 0 {
		t.Fatalf("stored=%d queried=%d: type absent but %d answer(s) returned (expected NODATA)",
			storedType, queriedType, got)
	}
}

// TestCustomDNSAnswerPathProperty exhaustively checks the present/absent property
// over every (stored, queried) type pair.
func TestCustomDNSAnswerPathProperty(t *testing.T) {
	for _, st := range fuzzTypes {
		for _, qt := range fuzzTypes {
			checkCustomDNSAnswerPath(t, st, qt)
		}
	}
}

// FuzzCustomDNSAnswerPath fuzzes the stored RR type and queried qtype (indices
// into fuzzTypes) and asserts the same invariants on random combinations.
func FuzzCustomDNSAnswerPath(f *testing.F) {
	// Seed corpus: cover the matching-type case, a cross-type case, and CNAME.
	f.Add(uint8(0), uint8(0)) // A / A
	f.Add(uint8(0), uint8(1)) // A / AAAA
	f.Add(uint8(2), uint8(5)) // TXT / MX
	f.Add(uint8(4), uint8(0)) // CNAME / A (chase)
	f.Add(uint8(7), uint8(7)) // PTR / PTR
	f.Add(uint8(8), uint8(3)) // CAA / SRV

	f.Fuzz(func(t *testing.T, a, b uint8) {
		storedType := fuzzTypes[int(a)%len(fuzzTypes)]
		queriedType := fuzzTypes[int(b)%len(fuzzTypes)]
		checkCustomDNSAnswerPath(t, storedType, queriedType)
	})
}
