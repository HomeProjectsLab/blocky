package server

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Local DNS zone assembly + validation", func() {
	It("assembles good rows into a valid zone and auto-appends trailing dots", func() {
		text := assembleZone([]localDNSRow{
			{Name: "web.lan", Type: "A", TTL: 0, Value: "10.0.0.5"},
			{Name: "www.lan", Type: "CNAME", Value: "web.lan"},
			{Name: "lan", Type: "MX", Value: "10 mail.lan"},
			{Name: "greet.lan", Type: "TXT", Value: "hello world"},
		})

		Expect(text).Should(ContainSubstring("web.lan.\t3600\tIN\tA\t10.0.0.5"))
		Expect(text).Should(ContainSubstring("mail.lan.")) // MX target got a dot
		Expect(text).Should(ContainSubstring(`"hello world"`))

		bad, err := validateZone(text)
		Expect(err).Should(Succeed())
		Expect(bad).Should(BeEmpty())
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
