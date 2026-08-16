package util

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"reflect"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HTTP Util", func() {
	Describe("DefaultHTTPTransport", func() {
		It("returns a new transport", func() {
			a := DefaultHTTPTransport()
			Expect(a).Should(BeIdenticalTo(a))

			b := DefaultHTTPTransport()
			Expect(a).ShouldNot(BeIdenticalTo(b))
		})

		It("returns a copy of http.DefaultTransport", func() {
			Expect(cmp.Diff(
				DefaultHTTPTransport(), http.DefaultTransport,
				cmpopts.IgnoreUnexported(http.Transport{}),
				// Non nil func field comparers
				cmp.Comparer(cmpAsPtrs[func(context.Context, string, string) (net.Conn, error)]),
				cmp.Comparer(cmpAsPtrs[func(*http.Request) (*url.URL, error)]),
			)).Should(BeEmpty())
		})
	})

	Describe("HTTPClientIP", func() {
		It("extracts the IP from RemoteAddr", func() {
			r, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
			Expect(err).Should(Succeed())

			ip := net.IPv4allrouter
			r.RemoteAddr = net.JoinHostPort(ip.String(), "78954")

			Expect(HTTPClientIP(r)).Should(Equal(ip))
		})

		It("extracts the IP from RemoteAddr without a port", func() {
			r, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
			Expect(err).Should(Succeed())

			ip := net.IPv4allrouter
			r.RemoteAddr = ip.String()

			Expect(HTTPClientIP(r)).Should(Equal(ip))
		})

		// Regression: forwarding headers are client-controlled and must NOT be
		// trusted — honoring them lets a DoH client rotate its rate-limit
		// bucket and impersonate other devices for per-client blocking.
		It("ignores the X-Forwarded-For header", func() {
			r, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
			Expect(err).Should(Succeed())

			remoteIP := net.ParseIP("192.168.1.100")
			r.RemoteAddr = net.JoinHostPort(remoteIP.String(), "12345")
			r.Header.Set("X-Forwarded-For", "203.0.113.195, 70.41.3.18")

			Expect(HTTPClientIP(r)).Should(Equal(remoteIP))
		})

		It("ignores the RFC 7239 Forwarded header", func() {
			r, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
			Expect(err).Should(Succeed())

			remoteIP := net.ParseIP("192.168.1.100")
			r.RemoteAddr = net.JoinHostPort(remoteIP.String(), "12345")
			r.Header.Set("Forwarded", "for=192.0.2.43;proto=http;by=203.0.113.43")

			Expect(HTTPClientIP(r)).Should(Equal(remoteIP))
		})
	})
})

// Go and cmp don't define func comparisons, besides with nil.
// In practice we can just compare them as pointers.
// See https://github.com/google/go-cmp/issues/162
func cmpAsPtrs[T any](x, y T) bool {
	return reflect.ValueOf(x).Pointer() == reflect.ValueOf(y).Pointer()
}
