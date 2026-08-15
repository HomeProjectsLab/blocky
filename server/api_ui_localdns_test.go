package server

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func u32(v uint32) *uint32 { return &v }

var _ = Describe("Local DNS zone assembly + validation", func() {
	It("assembles good rows into a valid zone and auto-appends trailing dots", func() {
		text := assembleZone([]localDNSRow{
			{Name: "web.lan", Type: "A", TTL: nil, Value: "10.0.0.5"},
			{Name: "www.lan", Type: "CNAME", Value: "web.lan"},
			{Name: "lan", Type: "MX", Value: "10 mail.lan"},
			{Name: "greet.lan", Type: "TXT", Value: "hello world"},
		})

		Expect(text).Should(ContainSubstring("web.lan.\t3600\tIN\tA\t10.0.0.5")) // nil TTL -> default 3600
		Expect(text).Should(ContainSubstring("mail.lan."))                       // MX target got a dot
		Expect(text).Should(ContainSubstring(`"hello world"`))

		bad, err := validateZone(text)
		Expect(err).Should(Succeed())
		Expect(bad).Should(BeEmpty())
	})

	It("honors an explicit TTL of 0 (do-not-cache) instead of forcing 3600", func() {
		text := assembleZone([]localDNSRow{
			{Name: "nocache.lan", Type: "A", TTL: u32(0), Value: "10.0.0.5"},
		})

		Expect(text).Should(ContainSubstring("nocache.lan.\t0\tIN\tA\t10.0.0.5"))
		Expect(text).ShouldNot(ContainSubstring("3600"))
	})

	It("preserves an explicit non-zero TTL", func() {
		text := assembleZone([]localDNSRow{
			{Name: "cached.lan", Type: "A", TTL: u32(120), Value: "10.0.0.5"},
		})

		Expect(text).Should(ContainSubstring("cached.lan.\t120\tIN\tA\t10.0.0.5"))
	})

	It("rejects a malformed record", func() {
		text := assembleZone([]localDNSRow{
			{Name: "ok.lan", Type: "A", Value: "10.0.0.1"},
			{Name: "bad.lan", Type: "A", Value: "not-an-ip"},
		})

		_, err := validateZone(text)
		Expect(err).Should(HaveOccurred())
	})
})
