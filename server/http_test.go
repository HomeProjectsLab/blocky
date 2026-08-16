package server

import (
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HTTP middleware", func() {
	var handler http.Handler

	BeforeEach(func() {
		handler = withCommonMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	})

	Describe("CORS", func() {
		preflight := func(origin string, headers map[string]string) *http.Response {
			req := httptest.NewRequest(http.MethodOptions, "http://blocky.lan/api/blocking/disable", nil)
			req.Host = "blocky.lan"
			req.Header.Set("Origin", origin)
			req.Header.Set("Access-Control-Request-Method", http.MethodGet)

			for k, v := range headers {
				req.Header.Set(k, v)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			return rec.Result()
		}

		// Regression: a wildcard origin (plus AllowPrivateNetwork) let any
		// website's JS drive the API cross-origin as a drive-by.
		It("should deny a cross-origin preflight", func() {
			res := preflight("https://evil.example.com", nil)

			Expect(res.Header.Get("Access-Control-Allow-Origin")).Should(BeEmpty())
		})

		It("should not grant Private Network Access to foreign origins", func() {
			res := preflight("https://evil.example.com",
				map[string]string{"Access-Control-Request-Private-Network": "true"})

			Expect(res.Header.Get("Access-Control-Allow-Private-Network")).Should(BeEmpty())
		})

		It("should answer a same-origin preflight", func() {
			res := preflight("http://blocky.lan", map[string]string{
				"Access-Control-Request-Headers": "content-type,x-custom-header",
			})

			Expect(res.Header.Get("Access-Control-Allow-Origin")).Should(Equal("http://blocky.lan"))
			Expect(res.Header.Get("Access-Control-Allow-Methods")).Should(ContainSubstring(http.MethodGet))
			// rs/cors answers a wildcard header allowlist by echoing the requested headers
			Expect(res.Header.Get("Access-Control-Allow-Headers")).Should(ContainSubstring("x-custom-header"))
		})
	})
})
