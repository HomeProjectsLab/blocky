package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	"github.com/0xERR0R/blocky/configstore"
	"github.com/0xERR0R/blocky/querylog"

	"github.com/go-chi/chi/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type fakeBlStats struct {
	stats []querylog.BlocklistStat
}

func (f *fakeBlStats) BlocklistCategories() ([]querylog.BlocklistStat, error) {
	return f.stats, nil
}

var _ = Describe("Blocking UI API", func() {
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

	BeforeEach(func() {
		var err error
		store, err = configstore.Open(GinkgoT().TempDir())
		Expect(err).Should(Succeed())
		DeferCleanup(store.Close)

		router = chi.NewRouter()
		registerBlockingUIEndpoints(router, store, &fakeBlStats{stats: []querylog.BlocklistStat{
			{Name: "ads", Count: 234025},
		}})
	})

	Describe("GET /api/ui/blocking", func() {
		It("returns categories with counts, segments and manual entries", func() {
			Expect(store.SetClientSegment("kids", []string{"porn"})).Should(Succeed())

			_, err := store.AddDenyEntry("manual", "bad.example.com")
			Expect(err).Should(Succeed())

			rec := exec(http.MethodGet, "/api/ui/blocking", "")
			Expect(rec.Code).Should(Equal(http.StatusOK))

			body := jsonBody(rec)

			cats := body["categories"].([]any)
			Expect(cats).ShouldNot(BeEmpty())

			var ads map[string]any

			for _, c := range cats {
				cm := c.(map[string]any)
				if cm["name"] == "ads" {
					ads = cm
				}
			}

			Expect(ads).ShouldNot(BeNil())
			Expect(ads["enabled"]).Should(BeTrue())
			Expect(ads["default"]).Should(BeTrue())
			Expect(ads["domains"]).Should(BeNumerically("==", 234025))

			segs := body["segments"].([]any)
			Expect(segs).Should(HaveLen(1))
			Expect(segs[0].(map[string]any)["client"]).Should(Equal("kids"))

			deny := body["deny"].([]any)
			Expect(deny).Should(HaveLen(1))
			Expect(deny[0].(map[string]any)["domain"]).Should(Equal("bad.example.com"))
			Expect(body["allow"].([]any)).Should(BeEmpty())
		})

		It("responds 503 without a store", func() {
			router = chi.NewRouter()
			registerBlockingUIEndpoints(router, nil, nil)

			rec := exec(http.MethodGet, "/api/ui/blocking", "")
			Expect(rec.Code).Should(Equal(http.StatusServiceUnavailable))
		})
	})

	Describe("PUT /api/ui/blocking/categories/{name}", func() {
		It("toggles a category and reports needsApply", func() {
			rec := exec(http.MethodPut, "/api/ui/blocking/categories/porn", `{"enable":true}`)
			Expect(rec.Code).Should(Equal(http.StatusOK))
			Expect(jsonBody(rec)["needsApply"]).Should(BeTrue())

			cats, err := store.ListBlockingCategories()
			Expect(err).Should(Succeed())

			for _, c := range cats {
				if c.Name == "porn" {
					Expect(c.Enabled).Should(BeTrue())
				}
			}
		})

		It("rejects an unknown category with 400", func() {
			rec := exec(http.MethodPut, "/api/ui/blocking/categories/nope", `{"enable":true}`)
			Expect(rec.Code).Should(Equal(http.StatusBadRequest))
			Expect(jsonBody(rec)["error"]).Should(ContainSubstring("unknown"))
		})
	})

	Describe("PUT /api/ui/blocking/segments/{client}", func() {
		It("stores the segment and reports needsApply", func() {
			rec := exec(http.MethodPut, "/api/ui/blocking/segments/kids-tablet",
				`{"categories":["porn","gambling"]}`)
			Expect(rec.Code).Should(Equal(http.StatusOK))
			Expect(jsonBody(rec)["needsApply"]).Should(BeTrue())

			segs, err := store.GetClientSegments()
			Expect(err).Should(Succeed())
			Expect(segs["kids-tablet"]).Should(ConsistOf("porn", "gambling"))
		})

		It("clears a segment with an empty category list", func() {
			Expect(store.SetClientSegment("kids-tablet", []string{"porn"})).Should(Succeed())

			rec := exec(http.MethodPut, "/api/ui/blocking/segments/kids-tablet", `{"categories":[]}`)
			Expect(rec.Code).Should(Equal(http.StatusOK))

			segs, err := store.GetClientSegments()
			Expect(err).Should(Succeed())
			Expect(segs).ShouldNot(HaveKey("kids-tablet"))
		})

		It("rejects unknown categories with 400", func() {
			rec := exec(http.MethodPut, "/api/ui/blocking/segments/kids", `{"categories":["nope"]}`)
			Expect(rec.Code).Should(Equal(http.StatusBadRequest))
		})
	})

	Describe("allow/deny CRUD", func() {
		It("adds and deletes a deny entry", func() {
			rec := exec(http.MethodPost, "/api/ui/blocking/deny", `{"domain":"bad.example.com"}`)
			Expect(rec.Code).Should(Equal(http.StatusOK))

			body := jsonBody(rec)
			Expect(body["needsApply"]).Should(BeTrue())
			Expect(body["added"]).Should(BeNumerically("==", 1))

			id := int(body["ids"].([]any)[0].(float64))
			Expect(id).Should(BeNumerically(">", 0))

			rec = exec(http.MethodDelete, "/api/ui/blocking/deny/"+strconv.Itoa(id), "")
			Expect(rec.Code).Should(Equal(http.StatusOK))

			denies, err := store.ListDenyEntries()
			Expect(err).Should(Succeed())
			Expect(denies).Should(BeEmpty())
		})

		It("adds a whole pasted list at once (space/comma/newline, strips URL paths)", func() {
			rec := exec(http.MethodPost, "/api/ui/blocking/deny",
				`{"domain":"a.com b.com,c.com\nd.com/gampad/ads"}`)
			Expect(rec.Code).Should(Equal(http.StatusOK))
			Expect(jsonBody(rec)["added"]).Should(BeNumerically("==", 4))

			denies, err := store.ListDenyEntries()
			Expect(err).Should(Succeed())
			domains := make([]string, len(denies))
			for i, d := range denies {
				domains[i] = d.Domain
			}
			Expect(domains).Should(ConsistOf("a.com", "b.com", "c.com", "d.com")) // path stripped
		})

		It("adds an allow entry", func() {
			rec := exec(http.MethodPost, "/api/ui/blocking/allow", `{"domain":"good.example.com"}`)
			Expect(rec.Code).Should(Equal(http.StatusOK))

			allows, err := store.ListAllowEntries()
			Expect(err).Should(Succeed())
			Expect(allows).Should(HaveLen(1))
			Expect(allows[0].Domain).Should(Equal("good.example.com"))
		})

		It("rejects input with no valid domains with 400", func() {
			rec := exec(http.MethodPost, "/api/ui/blocking/deny", `{"domain":"   "}`)
			Expect(rec.Code).Should(Equal(http.StatusBadRequest))
		})

		It("rejects a non-numeric id with 400", func() {
			rec := exec(http.MethodDelete, "/api/ui/blocking/deny/abc", "")
			Expect(rec.Code).Should(Equal(http.StatusBadRequest))
		})
	})
})
