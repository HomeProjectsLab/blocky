package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/configstore"

	"github.com/go-chi/chi/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type fakeSwapper struct {
	err   error
	group string
	got   []config.Upstream
}

func (f *fakeSwapper) SwapUpstreams(_ context.Context, group string, upstreams []config.Upstream) error {
	f.group = group
	f.got = upstreams

	return f.err
}

var _ = Describe("Upstreams UI API", func() {
	var (
		store   *configstore.Store
		swapper *fakeSwapper
		router  *chi.Mux
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

		swapper = &fakeSwapper{}
		router = chi.NewRouter()
		registerConfigUIEndpoints(router, store, swapper)
	})

	Describe("GET /api/ui/upstreams", func() {
		It("returns an empty group list while tables are empty", func() {
			rec := exec(http.MethodGet, "/api/ui/upstreams", "")

			Expect(rec.Code).Should(Equal(http.StatusOK))
			Expect(jsonBody(rec)["groups"]).Should(BeEmpty())
		})

		It("returns groups with their entries", func() {
			Expect(store.PutUpstreamGroup(configstore.UpstreamGroup{Name: "default", Strategy: "round_robin"})).
				Should(Succeed())
			Expect(store.SetUpstreamEntries("default", []configstore.UpstreamEntry{
				{Address: "1.1.1.1", Weight: 2, Enabled: true},
			})).Should(Succeed())

			rec := exec(http.MethodGet, "/api/ui/upstreams", "")
			Expect(rec.Code).Should(Equal(http.StatusOK))

			groups := jsonBody(rec)["groups"].([]any)
			Expect(groups).Should(HaveLen(1))

			group := groups[0].(map[string]any)
			Expect(group["name"]).Should(Equal("default"))
			Expect(group["strategy"]).Should(Equal("round_robin"))

			entries := group["entries"].([]any)
			Expect(entries).Should(HaveLen(1))

			entry := entries[0].(map[string]any)
			Expect(entry["address"]).Should(Equal("1.1.1.1"))
			Expect(entry["weight"]).Should(BeNumerically("==", 2))
			Expect(entry["enabled"]).Should(BeTrue())
			Expect(entry["position"]).Should(BeNumerically("==", 0))
		})
	})

	Describe("PUT /api/ui/upstreams/groups/{name}", func() {
		It("upserts group meta and reports needsApply", func() {
			rec := exec(http.MethodPut, "/api/ui/upstreams/groups/default",
				`{"strategy":"time_hop","hopMin":"1m","hopMax":"10m"}`)

			Expect(rec.Code).Should(Equal(http.StatusOK))
			Expect(jsonBody(rec)["needsApply"]).Should(BeTrue())

			groups, _, err := store.ListUpstreamGroups()
			Expect(err).Should(Succeed())
			Expect(groups).Should(HaveLen(1))
			Expect(groups[0].Strategy).Should(Equal("time_hop"))
		})

		It("rejects an invalid strategy with 400", func() {
			rec := exec(http.MethodPut, "/api/ui/upstreams/groups/default", `{"strategy":"quantum"}`)

			Expect(rec.Code).Should(Equal(http.StatusBadRequest))
			Expect(jsonBody(rec)).Should(HaveKey("error"))
		})

		It("rejects invalid hop settings with 400", func() {
			rec := exec(http.MethodPut, "/api/ui/upstreams/groups/default",
				`{"strategy":"time_hop","hopMin":"1h","hopMax":"1m"}`)

			Expect(rec.Code).Should(Equal(http.StatusBadRequest))
			Expect(jsonBody(rec)["error"]).Should(ContainSubstring("hopMin"))
		})
	})

	Describe("DELETE /api/ui/upstreams/groups/{name}", func() {
		It("refuses to delete the default group with 400", func() {
			rec := exec(http.MethodDelete, "/api/ui/upstreams/groups/default", "")

			Expect(rec.Code).Should(Equal(http.StatusBadRequest))
			Expect(jsonBody(rec)["error"]).Should(ContainSubstring("default"))
		})

		It("deletes another group with 204", func() {
			Expect(store.PutUpstreamGroup(configstore.UpstreamGroup{Name: "kids"})).Should(Succeed())

			rec := exec(http.MethodDelete, "/api/ui/upstreams/groups/kids", "")

			Expect(rec.Code).Should(Equal(http.StatusNoContent))
		})
	})

	Describe("PUT /api/ui/upstreams/groups/{name}/entries", func() {
		BeforeEach(func() {
			Expect(store.PutUpstreamGroup(configstore.UpstreamGroup{Name: "default"})).Should(Succeed())
		})

		It("persists entries and reports swapped on success", func() {
			rec := exec(http.MethodPut, "/api/ui/upstreams/groups/default/entries",
				`{"entries":[{"address":"1.1.1.1","weight":3},{"address":"9.9.9.9","enabled":false}]}`)

			Expect(rec.Code).Should(Equal(http.StatusOK))
			Expect(jsonBody(rec)["swapped"]).Should(BeTrue())

			// swap got only the enabled entry, with its weight
			Expect(swapper.group).Should(Equal("default"))
			Expect(swapper.got).Should(HaveLen(1))
			Expect(swapper.got[0].Host).Should(Equal("1.1.1.1"))
			Expect(swapper.got[0].Weight).Should(Equal(uint(3)))

			// enabled defaults to true when absent
			_, entries, err := store.ListUpstreamGroups()
			Expect(err).Should(Succeed())
			Expect(entries["default"][0].Enabled).Should(BeTrue())
			Expect(entries["default"][1].Enabled).Should(BeFalse())
		})

		It("persists but reports needsApply when the swap fails", func() {
			swapper.err = errors.New("no upstream tree in the running resolver chain")

			rec := exec(http.MethodPut, "/api/ui/upstreams/groups/default/entries",
				`{"entries":[{"address":"1.1.1.1"}]}`)

			Expect(rec.Code).Should(Equal(http.StatusOK))
			body := jsonBody(rec)
			Expect(body["swapped"]).Should(BeFalse())
			Expect(body["needsApply"]).Should(BeTrue())
			Expect(body["reason"]).Should(ContainSubstring("no upstream tree"))

			// persisted regardless
			_, entries, err := store.ListUpstreamGroups()
			Expect(err).Should(Succeed())
			Expect(entries["default"]).Should(HaveLen(1))
		})

		It("reports needsApply when no swapper is wired", func() {
			router = chi.NewRouter()
			registerConfigUIEndpoints(router, store, nil)

			rec := exec(http.MethodPut, "/api/ui/upstreams/groups/default/entries",
				`{"entries":[{"address":"1.1.1.1"}]}`)

			Expect(rec.Code).Should(Equal(http.StatusOK))
			body := jsonBody(rec)
			Expect(body["swapped"]).Should(BeFalse())
			Expect(body["needsApply"]).Should(BeTrue())
		})

		It("rejects a garbage address with 400 and leaves the DB untouched", func() {
			rec := exec(http.MethodPut, "/api/ui/upstreams/groups/default/entries",
				`{"entries":[{"address":"not a::valid///upstream"}]}`)

			Expect(rec.Code).Should(Equal(http.StatusBadRequest))
			Expect(jsonBody(rec)).Should(HaveKey("error"))

			_, entries, err := store.ListUpstreamGroups()
			Expect(err).Should(Succeed())
			Expect(entries["default"]).Should(BeEmpty())
		})

		It("rejects an unknown group with 400", func() {
			rec := exec(http.MethodPut, "/api/ui/upstreams/groups/nope/entries",
				`{"entries":[{"address":"1.1.1.1"}]}`)

			Expect(rec.Code).Should(Equal(http.StatusBadRequest))
			Expect(jsonBody(rec)["error"]).Should(ContainSubstring("unknown upstream group"))
		})

		It("responds 503 without a store", func() {
			router = chi.NewRouter()
			registerConfigUIEndpoints(router, nil, nil)

			rec := exec(http.MethodPut, "/api/ui/upstreams/groups/default/entries", `{"entries":[]}`)

			Expect(rec.Code).Should(Equal(http.StatusServiceUnavailable))
		})
	})
})
