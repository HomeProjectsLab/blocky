package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/querylog"

	"github.com/go-chi/chi/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Noise (decoy) UI API", func() {
	var router *chi.Mux

	exec := func(path string) *httptest.ResponseRecorder {
		GinkgoHelper()

		req := httptest.NewRequest(http.MethodGet, path, nil)
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

	decoyEntry := func(start time.Time, question, source string) *querylog.LogEntry {
		return &querylog.LogEntry{
			Start:        start,
			ClientIP:     "192.168.1.10",
			ClientNames:  []string{"laptop"},
			QuestionName: question,
			QuestionType: "A",
			ResponseType: "RESOLVED",
			ResponseCode: "NOERROR",
			DurationMs:   5,
			Fingerprint:  model.Fingerprint{Transport: model.TransportDo53UDP},
			Decoy:        true,
			DecoySource:  source,
		}
	}

	BeforeEach(func() {
		ctx, cancelFn := context.WithCancel(context.Background())
		DeferCleanup(cancelFn)

		dbPath := filepath.Join(GinkgoT().TempDir(), "querylog.db")

		writer, err := querylog.NewDatabaseWriter(ctx, config.QueryLogTypeSqlite, dbPath, 7, time.Minute)
		Expect(err).Should(Succeed())

		base := time.Now().Add(-time.Hour)
		// a real row (must be excluded from every decoy reader)
		writer.Write(&querylog.LogEntry{
			Start: base, ClientIP: "192.168.1.11", ClientNames: []string{"phone"},
			QuestionName: "real.example.com.", QuestionType: "A", ResponseType: "RESOLVED",
			ResponseCode: "NOERROR", DurationMs: 3, Fingerprint: model.Fingerprint{Transport: model.TransportDo53UDP},
		})
		// decoys: replay x2 (alpha.com), corpus x1 (beta.net)
		writer.Write(decoyEntry(base.Add(time.Minute), "www.alpha.com.", "replay"))
		writer.Write(decoyEntry(base.Add(2*time.Minute), "cdn.alpha.com.", "replay"))
		writer.Write(decoyEntry(base.Add(3*time.Minute), "api.beta.net.", "corpus"))
		Expect(writer.Flush()).Should(Succeed())

		cfg := &config.Config{}
		cfg.QueryLog = config.QueryLog{Type: config.QueryLogTypeSqlite, Target: config.Secret(dbPath)}

		router = chi.NewRouter()
		registerStatsUIEndpoints(router, cfg, querylog.NewHub(), nil)
	})

	It("overview counts decoys, distinct fake domains and source mix (excludes real)", func() {
		body := jsonBody(exec("/api/ui/noise/overview"))
		Expect(body).Should(HaveKeyWithValue("decoys", BeNumerically("==", 3)))
		Expect(body).Should(HaveKeyWithValue("distinctDomains", BeNumerically("==", 2)))
		Expect(body["bySource"]).Should(HaveKeyWithValue("replay", BeNumerically("==", 2)))
		Expect(body["bySource"]).Should(HaveKeyWithValue("corpus", BeNumerically("==", 1)))
	})

	It("sourcemix returns per-source counts, most frequent first", func() {
		items := jsonBody(exec("/api/ui/noise/sourcemix"))["items"].([]any)
		Expect(items).Should(HaveLen(2))
		first := items[0].(map[string]any)
		Expect(first).Should(HaveKeyWithValue("name", "replay"))
		Expect(first).Should(HaveKeyWithValue("count", BeNumerically("==", 2)))
	})

	It("top returns fake domains by eTLD+1", func() {
		items := jsonBody(exec("/api/ui/noise/top"))["items"].([]any)
		first := items[0].(map[string]any)
		Expect(first).Should(HaveKeyWithValue("name", "alpha.com"))
		Expect(first).Should(HaveKeyWithValue("count", BeNumerically("==", 2)))
	})

	It("buckets split decoy counts by source", func() {
		buckets := jsonBody(exec("/api/ui/noise/buckets?step=3600"))["buckets"].([]any)
		Expect(buckets).ShouldNot(BeEmpty())
		total := map[string]float64{}
		for _, b := range buckets {
			for k, v := range b.(map[string]any)["counts"].(map[string]any) {
				total[k] += v.(float64)
			}
		}
		Expect(total["replay"]).Should(BeNumerically("==", 2))
		Expect(total["corpus"]).Should(BeNumerically("==", 1))
	})
})
