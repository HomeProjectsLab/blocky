package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/0xERR0R/blocky/configstore"

	"github.com/go-chi/chi/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Config UI API", func() {
	var (
		store  *configstore.Store
		router *chi.Mux
	)

	exec := func(method, path, body string) *httptest.ResponseRecorder {
		GinkgoHelper()

		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		return rec
	}

	jsonBody := func(rec *httptest.ResponseRecorder) map[string]any {
		GinkgoHelper()

		var m map[string]any
		Expect(json.Unmarshal(rec.Body.Bytes(), &m)).Should(Succeed())

		return m
	}

	const validYAML = "ports:\n  http: 5000\nupstreams:\n  groups:\n    default:\n      - 1.1.1.1\n"

	BeforeEach(func() {
		var err error
		store, err = configstore.Open(GinkgoT().TempDir())
		Expect(err).Should(Succeed())
		DeferCleanup(store.Close)

		router = chi.NewRouter()
		registerConfigUIEndpoints(router, store)
	})

	Describe("GET /api/ui/config/raw", func() {
		It("returns the seeded YAML blob", func() {
			rec := exec(http.MethodGet, "/api/ui/config/raw", "")

			Expect(rec.Code).Should(Equal(http.StatusOK))
			Expect(rec.Header().Get(contentTypeHeader)).Should(Equal(yamlContentType))
			Expect(rec.Body.String()).Should(ContainSubstring("9.9.9.9"))
		})
	})

	Describe("PUT /api/ui/config/raw", func() {
		It("rejects invalid YAML with 400 and keeps the stored blob", func() {
			before, err := store.RawYAML()
			Expect(err).Should(Succeed())

			rec := exec(http.MethodPut, "/api/ui/config/raw", "upstreams: [not, a, map")

			Expect(rec.Code).Should(Equal(http.StatusBadRequest))
			Expect(rec.Header().Get(contentTypeHeader)).Should(Equal(jsonContentType))
			Expect(jsonBody(rec)).Should(HaveKey("error"))

			after, err := store.RawYAML()
			Expect(err).Should(Succeed())
			Expect(after).Should(Equal(before))
		})

		It("persists valid YAML with 204 and GET reflects it", func() {
			rec := exec(http.MethodPut, "/api/ui/config/raw", validYAML)
			Expect(rec.Code).Should(Equal(http.StatusNoContent))

			get := exec(http.MethodGet, "/api/ui/config/raw", "")
			Expect(get.Body.String()).Should(Equal(validYAML))
		})
	})

	Describe("POST /api/ui/config/validate", func() {
		It("returns valid=true for valid YAML", func() {
			rec := exec(http.MethodPost, "/api/ui/config/validate", validYAML)

			Expect(rec.Code).Should(Equal(http.StatusOK))
			Expect(jsonBody(rec)).Should(HaveKeyWithValue("valid", true))
		})

		It("returns valid=false plus error for invalid YAML", func() {
			rec := exec(http.MethodPost, "/api/ui/config/validate", "definitelyNotAKey: true")

			Expect(rec.Code).Should(Equal(http.StatusOK))
			body := jsonBody(rec)
			Expect(body).Should(HaveKeyWithValue("valid", false))
			Expect(body["error"]).ShouldNot(BeEmpty())
		})

		It("validates the stored blob on empty body", func() {
			rec := exec(http.MethodPost, "/api/ui/config/validate", "")

			Expect(rec.Code).Should(Equal(http.StatusOK))
			Expect(jsonBody(rec)).Should(HaveKeyWithValue("valid", true))
		})
	})

	Describe("POST /api/ui/config/apply", func() {
		It("returns 202 and signals ApplyRequested", func() {
			rec := exec(http.MethodPost, "/api/ui/config/apply", "")

			Expect(rec.Code).Should(Equal(http.StatusAccepted))
			Expect(jsonBody(rec)).Should(HaveKeyWithValue("status", "applying"))
			Expect(store.ApplyRequested()).Should(Receive())
		})
	})

	Describe("GET /api/ui/config/status", func() {
		It("is dirty before apply, clean after MarkApplied, dirty after PUT", func() {
			rec := exec(http.MethodGet, "/api/ui/config/status", "")
			Expect(rec.Code).Should(Equal(http.StatusOK))
			body := jsonBody(rec)
			Expect(body).Should(HaveKeyWithValue("dirty", true))
			Expect(body["lastApplied"]).Should(BeNil())
			Expect(body["updatedAt"]).ShouldNot(BeEmpty())

			store.MarkApplied()

			body = jsonBody(exec(http.MethodGet, "/api/ui/config/status", ""))
			Expect(body).Should(HaveKeyWithValue("dirty", false))
			Expect(body["lastApplied"]).ShouldNot(BeNil())

			Expect(exec(http.MethodPut, "/api/ui/config/raw", validYAML).Code).Should(Equal(http.StatusNoContent))

			body = jsonBody(exec(http.MethodGet, "/api/ui/config/status", ""))
			Expect(body).Should(HaveKeyWithValue("dirty", true))
		})
	})

	Describe("nil store", func() {
		It("responds 503 on every endpoint", func() {
			router = chi.NewRouter()
			registerConfigUIEndpoints(router, nil)

			for _, tc := range []struct{ method, path string }{
				{http.MethodGet, "/api/ui/config/raw"},
				{http.MethodPut, "/api/ui/config/raw"},
				{http.MethodPost, "/api/ui/config/validate"},
				{http.MethodPost, "/api/ui/config/apply"},
				{http.MethodGet, "/api/ui/config/status"},
			} {
				rec := exec(tc.method, tc.path, "")
				Expect(rec.Code).Should(Equal(http.StatusServiceUnavailable), tc.method+" "+tc.path)
			}
		})
	})
})
