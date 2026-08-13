package server

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"

	"github.com/0xERR0R/blocky/model"
	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeResponseWriter is a minimal dns.ResponseWriter (+ optional TLS state) for
// request-construction tests.
type fakeResponseWriter struct {
	remoteAddr net.Addr
	tlsState   *tls.ConnectionState
}

func (f *fakeResponseWriter) LocalAddr() net.Addr                   { return nil }
func (f *fakeResponseWriter) RemoteAddr() net.Addr                  { return f.remoteAddr }
func (f *fakeResponseWriter) WriteMsg(*dns.Msg) error               { return nil }
func (f *fakeResponseWriter) Write([]byte) (int, error)             { return 0, nil }
func (f *fakeResponseWriter) Close() error                          { return nil }
func (f *fakeResponseWriter) TsigStatus() error                     { return nil }
func (f *fakeResponseWriter) TsigTimersOnly(bool)                   {}
func (f *fakeResponseWriter) Hijack()                               {}
func (f *fakeResponseWriter) ConnectionState() *tls.ConnectionState { return f.tlsState }

var _ = Describe("request fingerprint capture", func() {
	newMsg := func() *dns.Msg {
		msg := new(dns.Msg)
		msg.SetQuestion("example.com.", dns.TypeA)
		msg.Id = 4711
		msg.SetEdns0(1232, true)

		return msg
	}

	Describe("fingerprintFromMsg", func() {
		It("captures message-level attributes", func() {
			msg := newMsg()
			opt := msg.IsEdns0()
			opt.Option = append(opt.Option,
				&dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: "24"},
				&dns.EDNS0_PADDING{})

			fp := fingerprintFromMsg(msg)

			Expect(fp.MsgID).Should(Equal(uint16(4711)))
			Expect(fp.QClass).Should(Equal(uint16(dns.ClassINET)))
			Expect(fp.RD).Should(BeTrue())
			Expect(fp.HadEDNS0).Should(BeTrue())
			Expect(fp.EDNSUDPSize).Should(Equal(uint16(1232)))
			Expect(fp.DO).Should(BeTrue())
			Expect(fp.EDNSOptCodes).Should(Equal([]uint16{dns.EDNS0COOKIE, dns.EDNS0PADDING}))
			Expect(fp.HasCookie).Should(BeTrue())
			Expect(fp.Mixed0x20).Should(BeFalse())
		})

		It("detects 0x20 mixed casing", func() {
			msg := new(dns.Msg)
			msg.SetQuestion("eXaMpLe.CoM.", dns.TypeA)

			Expect(fingerprintFromMsg(msg).Mixed0x20).Should(BeTrue())
		})

		It("handles messages without EDNS0", func() {
			msg := new(dns.Msg)
			msg.SetQuestion("example.com.", dns.TypeA)

			fp := fingerprintFromMsg(msg)
			Expect(fp.HadEDNS0).Should(BeFalse())
			Expect(fp.EDNSOptCodes).Should(BeEmpty())
		})
	})

	Describe("newRequestFromDNS", func() {
		It("captures Do53 UDP transport and source port", func() {
			rw := &fakeResponseWriter{
				remoteAddr: &net.UDPAddr{IP: net.ParseIP("192.168.178.88"), Port: 40123},
			}

			_, request := newRequestFromDNS(context.Background(), rw, newMsg())

			Expect(request.Fingerprint.Transport).Should(Equal(model.TransportDo53UDP))
			Expect(request.Fingerprint.SrcPort).Should(Equal(uint16(40123)))
		})

		It("captures Do53 TCP transport", func() {
			rw := &fakeResponseWriter{
				remoteAddr: &net.TCPAddr{IP: net.ParseIP("192.168.178.88"), Port: 40124},
			}

			_, request := newRequestFromDNS(context.Background(), rw, newMsg())

			Expect(request.Fingerprint.Transport).Should(Equal(model.TransportDo53TCP))
			Expect(request.Fingerprint.SrcPort).Should(Equal(uint16(40124)))
		})

		It("captures DoT transport with TLS attributes", func() {
			rw := &fakeResponseWriter{
				remoteAddr: &net.TCPAddr{IP: net.ParseIP("192.168.178.88"), Port: 40125},
				tlsState: &tls.ConnectionState{
					Version:            tls.VersionTLS13,
					CipherSuite:        tls.TLS_AES_128_GCM_SHA256,
					ServerName:         "dns.example.com",
					NegotiatedProtocol: "dot",
				},
			}

			_, request := newRequestFromDNS(context.Background(), rw, newMsg())

			fp := request.Fingerprint
			Expect(fp.Transport).Should(Equal(model.TransportDoT))
			Expect(fp.TLSVersion).Should(Equal(uint16(tls.VersionTLS13)))
			Expect(fp.TLSCipher).Should(Equal(uint16(tls.TLS_AES_128_GCM_SHA256)))
			Expect(fp.SNI).Should(Equal("dns.example.com"))
			Expect(fp.ALPN).Should(Equal("dot"))
			Expect(fp.SrcPort).Should(Equal(uint16(40125)))
		})
	})

	Describe("newRequestFromHTTP", func() {
		It("captures DoH transport, TLS attributes and user agent", func() {
			httpReq := httptest.NewRequest(http.MethodPost, "https://dns.example.com/dns-query", nil)
			httpReq.RemoteAddr = "192.168.178.88:40126"
			httpReq.Header.Set("User-Agent", "test-doh-client/1.0")
			httpReq.TLS = &tls.ConnectionState{
				Version:            tls.VersionTLS13,
				CipherSuite:        tls.TLS_CHACHA20_POLY1305_SHA256,
				ServerName:         "dns.example.com",
				NegotiatedProtocol: "h2",
			}

			_, request := newRequestFromHTTP(context.Background(), httpReq, newMsg())

			fp := request.Fingerprint
			Expect(fp.Transport).Should(Equal(model.TransportDoH))
			Expect(fp.TLSVersion).Should(Equal(uint16(tls.VersionTLS13)))
			Expect(fp.TLSCipher).Should(Equal(uint16(tls.TLS_CHACHA20_POLY1305_SHA256)))
			Expect(fp.SNI).Should(Equal("dns.example.com"))
			Expect(fp.ALPN).Should(Equal("h2"))
			Expect(fp.UserAgent).Should(Equal("test-doh-client/1.0"))
			Expect(fp.SrcPort).Should(Equal(uint16(40126)))
		})

		It("captures DoH3 transport for HTTP/3 requests", func() {
			httpReq := httptest.NewRequest(http.MethodPost, "https://dns.example.com/dns-query", nil)
			httpReq.ProtoMajor = 3
			httpReq.RemoteAddr = "192.168.178.88:40127"

			_, request := newRequestFromHTTP(context.Background(), httpReq, newMsg())

			Expect(request.Fingerprint.Transport).Should(Equal(model.TransportDoH3))
		})
	})
})
