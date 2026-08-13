package server

import (
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/go-chi/chi/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Web UI shell", func() {
	var ts *httptest.Server

	BeforeEach(func() {
		router := chi.NewRouter()
		configureRootHandler(router)
		configureStaticAssetsHandler(router)

		ts = httptest.NewServer(router)
		DeferCleanup(ts.Close)
	})

	get := func(path string) (*http.Response, string) {
		res, err := http.Get(ts.URL + path)
		Expect(err).Should(Succeed())
		DeferCleanup(res.Body.Close)

		body, err := io.ReadAll(res.Body)
		Expect(err).Should(Succeed())

		return res, string(body)
	}

	Describe("shell routes", func() {
		for _, p := range uiPages {
			It("serves the shell for "+p.Route+" with page identity "+p.Page, func() {
				res, body := get(p.Route)

				Expect(res.StatusCode).Should(Equal(http.StatusOK))
				Expect(res.Header.Get("Content-Type")).Should(ContainSubstring("text/html"))
				Expect(body).Should(ContainSubstring(`data-page="` + p.Page + `"`))
				Expect(body).Should(ContainSubstring(`id="page-` + p.Page + `"`))
				Expect(body).Should(ContainSubstring("<nav"))
			})
		}
	})

	Describe("blocking page", func() {
		It("renders the ad-blocker management sections", func() {
			_, body := get("/blocking")

			for _, id := range []string{
				"bl-apply", "bl-cats", "bl-segments", "seg-add",
				"allow-in", "allow-list", "deny-in", "deny-list",
				"bt-blocked", "bt-rate", "bl-top", "bl-bygroup",
			} {
				Expect(body).Should(ContainSubstring(`id="` + id + `"`))
			}
		})
	})

	Describe("navigation", func() {
		It("lists every management page in the nav", func() {
			_, body := get("/")

			for _, label := range []string{"Clients", "Upstreams", "Blocking", "Privacy", "Settings", "System"} {
				Expect(body).Should(ContainSubstring(">" + label + "<"))
			}
		})
	})

	Describe("static assets", func() {
		for _, path := range []string{
			"/static/vendor/uplot.min.css",
			"/static/vendor/uplot.iife.min.js",
			"/static/app/app.css",
			"/static/app/app.js",
			"/static/app/upstreams.js",
			"/static/app/blocking.js",
			"/static/app/clients.js",
			"/static/app/privacy.js",
			"/static/app/settings.js",
			"/static/app/system.js",
		} {
			It("serves "+path, func() {
				res, body := get(path)

				Expect(res.StatusCode).Should(Equal(http.StatusOK))
				Expect(body).ShouldNot(BeEmpty())
			})
		}
	})
})
