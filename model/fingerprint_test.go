package model

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Fingerprint", func() {
	baseFp := func() Fingerprint {
		return Fingerprint{
			Transport:    TransportDoT,
			SrcPort:      54321,
			TLSVersion:   0x0304,
			TLSCipher:    0x1301,
			SNI:          "dns.example.com",
			ALPN:         "dot",
			MsgID:        1234,
			QClass:       1,
			RD:           true,
			HadEDNS0:     true,
			EDNSUDPSize:  1232,
			DO:           true,
			EDNSOptCodes: []uint16{10, 8, 12},
			HasCookie:    true,
			Mixed0x20:    false,
		}
	}

	Describe("Hash", func() {
		It("ignores per-query noise (MsgID, SrcPort, SNI, HasCookie, Mixed0x20)", func() {
			a := baseFp()
			b := baseFp()
			b.MsgID = 999
			b.SrcPort = 1
			b.SNI = "other.example.com"
			b.HasCookie = false
			b.Mixed0x20 = true

			Expect(b.Hash()).Should(Equal(a.Hash()))
		})

		It("changes when the EDNS option code order changes", func() {
			a := baseFp()
			b := baseFp()
			b.EDNSOptCodes = []uint16{8, 10, 12}

			Expect(b.Hash()).ShouldNot(Equal(a.Hash()))
		})

		It("changes when the EDNS UDP size changes", func() {
			a := baseFp()
			b := baseFp()
			b.EDNSUDPSize = 4096

			Expect(b.Hash()).ShouldNot(Equal(a.Hash()))
		})

		It("changes when the transport changes", func() {
			a := baseFp()
			b := baseFp()
			b.Transport = TransportDoH

			Expect(b.Hash()).ShouldNot(Equal(a.Hash()))
		})

		It("is deterministic", func() {
			a := baseFp()

			Expect(a.Hash()).Should(Equal(a.Hash()))
			Expect(a.Hash()).Should(HaveLen(20)) // 10 bytes hex
		})

		It("returns no hash below the entropy floor (bare Do53-UDP, no EDNS/TLS/UA)", func() {
			// A default stub query with nothing device-specific must NOT mint a
			// fp_hash, or every plain-DNS device on the LAN collapses into one key.
			bare := Fingerprint{Transport: TransportDo53UDP, RD: true, QClass: 1}
			Expect(bare.Hash()).Should(BeEmpty())

			// Any one discriminating signal lifts it over the floor.
			withEDNS := bare
			withEDNS.HadEDNS0 = true
			Expect(withEDNS.Hash()).ShouldNot(BeEmpty())

			withUA := bare
			withUA.UserAgent = "curl/8.0"
			Expect(withUA.Hash()).ShouldNot(BeEmpty())

			doh := bare
			doh.Transport = TransportDoH
			Expect(doh.Hash()).ShouldNot(BeEmpty())
		})
	})

	Describe("Transport String", func() {
		It("maps all values", func() {
			Expect(TransportDo53UDP.String()).Should(Equal("do53-udp"))
			Expect(TransportDo53TCP.String()).Should(Equal("do53-tcp"))
			Expect(TransportDoT.String()).Should(Equal("dot"))
			Expect(TransportDoH.String()).Should(Equal("doh"))
			Expect(TransportDoH3.String()).Should(Equal("doh3"))
		})
	})
})
