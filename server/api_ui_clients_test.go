package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/configstore"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/querylog"

	"github.com/go-chi/chi/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Clients + privacy UI API", func() {
	var router *chi.Mux

	exec := func(method, path string, body []byte) *httptest.ResponseRecorder {
		GinkgoHelper()

		var r *http.Request
		if body != nil {
			r = httptest.NewRequest(method, path, bytes.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)

		return rec
	}

	jsonBody := func(rec *httptest.ResponseRecorder) map[string]any {
		GinkgoHelper()

		var m map[string]any
		Expect(json.Unmarshal(rec.Body.Bytes(), &m)).Should(Succeed())

		return m
	}

	entry := func(start time.Time, client, question, rtype string, tr model.Transport) *querylog.LogEntry {
		return &querylog.LogEntry{
			Start:        start,
			ClientIP:     "192.168.1.10",
			ClientNames:  []string{client},
			QuestionName: question,
			QuestionType: "A",
			ResponseType: rtype,
			ResponseCode: "NOERROR",
			DurationMs:   12,
			Fingerprint:  model.Fingerprint{Transport: tr, HadEDNS0: true, EDNSUDPSize: 1232},
		}
	}

	BeforeEach(func() {
		ctx, cancel := context.WithCancel(context.Background())
		DeferCleanup(cancel)

		dir := GinkgoT().TempDir()
		dbPath := filepath.Join(dir, "querylog.db")

		writer, err := querylog.NewDatabaseWriter(ctx, config.QueryLogTypeSqlite, dbPath, 7, time.Minute)
		Expect(err).Should(Succeed())

		hour := time.Now().Add(-2 * time.Hour).Truncate(time.Hour)
		writer.Write(entry(hour.Add(time.Minute), "laptop", "www.example.com.", "RESOLVED", model.TransportDo53UDP))
		writer.Write(entry(hour.Add(2*time.Minute), "laptop", "ads.tracker.net.", "BLOCKED", model.TransportDo53UDP))
		writer.Write(entry(hour.Add(3*time.Minute), "phone", "www.example.com.", "CACHED", model.TransportDoT))
		Expect(writer.Flush()).Should(Succeed())

		cfg := &config.Config{}
		cfg.QueryLog = config.QueryLog{Type: config.QueryLogTypeSqlite, Target: config.Secret(dbPath)}

		store, err := configstore.Open(filepath.Join(dir, "cfg"))
		Expect(err).Should(Succeed())
		DeferCleanup(func() { _ = store.Close() })

		src, err := querylog.NewDecoySource(dbPath)
		Expect(err).Should(Succeed())
		DeferCleanup(func() { _ = src.Close() })

		router = chi.NewRouter()
		registerStatsUIEndpoints(router, cfg, querylog.NewHub(), store, src)
	})

	Describe("GET /api/ui/clients", func() {
		It("ranks clients by query count", func() {
			rec := exec(http.MethodGet, "/api/ui/clients", nil)

			Expect(rec.Code).Should(Equal(http.StatusOK))
			clients := jsonBody(rec)["clients"].([]any)
			Expect(clients).Should(HaveLen(2))
			first := clients[0].(map[string]any)
			Expect(first).Should(HaveKeyWithValue("name", "laptop"))
			Expect(first).Should(HaveKeyWithValue("queries", BeNumerically("==", 2)))
			Expect(first).Should(HaveKeyWithValue("blocked", BeNumerically("==", 1)))
			Expect(first).Should(HaveKey("lastSeen"))
		})
	})

	Describe("GET /api/ui/clients/{name}", func() {
		It("returns the fingerprint drill-down", func() {
			rec := exec(http.MethodGet, "/api/ui/clients/laptop", nil)

			Expect(rec.Code).Should(Equal(http.StatusOK))
			body := jsonBody(rec)
			Expect(body).Should(HaveKeyWithValue("name", "laptop"))
			Expect(body).Should(HaveKeyWithValue("queries", BeNumerically("==", 2)))
			Expect(body["transports"]).ShouldNot(BeEmpty())
			Expect(body["fingerprints"]).ShouldNot(BeEmpty())
			fp := body["fingerprints"].([]any)[0].(map[string]any)
			Expect(fp).Should(HaveKey("fpHash"))
			Expect(fp).Should(HaveKeyWithValue("hadEdns0", true))
			Expect(fp).Should(HaveKeyWithValue("ednsUdpSize", BeNumerically("==", 1232)))
			Expect(body["topDomains"]).ShouldNot(BeEmpty())
		})
	})

	Describe("GET /api/ui/clients/classes", func() {
		It("lists auto-classified clients", func() {
			rec := exec(http.MethodGet, "/api/ui/clients/classes", nil)

			Expect(rec.Code).Should(Equal(http.StatusOK))
			classes := jsonBody(rec)["classes"].([]any)
			Expect(classes).Should(HaveLen(2)) // laptop + phone; <20 queries => unknown
			names := []string{}
			for _, c := range classes {
				m := c.(map[string]any)
				names = append(names, m["client"].(string))
				Expect(m).Should(HaveKeyWithValue("effective", "unknown"))
			}
			Expect(names).Should(ConsistOf("laptop", "phone"))
		})
	})

	Describe("PUT /api/ui/clients/classes/{client}", func() {
		It("sets then clears a manual override", func() {
			put := exec(http.MethodPut, "/api/ui/clients/classes/laptop", []byte(`{"class":"iot"}`))
			Expect(put.Code).Should(Equal(http.StatusNoContent))

			classes := jsonBody(exec(http.MethodGet, "/api/ui/clients/classes", nil))["classes"].([]any)
			var laptop map[string]any
			for _, c := range classes {
				if m := c.(map[string]any); m["client"] == "laptop" {
					laptop = m
				}
			}
			Expect(laptop).Should(HaveKeyWithValue("override", "iot"))
			Expect(laptop).Should(HaveKeyWithValue("effective", "iot"))

			clr := exec(http.MethodPut, "/api/ui/clients/classes/laptop", []byte(`{"class":"auto"}`))
			Expect(clr.Code).Should(Equal(http.StatusNoContent))

			classes = jsonBody(exec(http.MethodGet, "/api/ui/clients/classes", nil))["classes"].([]any)
			for _, c := range classes {
				if m := c.(map[string]any); m["client"] == "laptop" {
					Expect(m).Should(HaveKeyWithValue("override", ""))
				}
			}
		})

		It("rejects an invalid class", func() {
			put := exec(http.MethodPut, "/api/ui/clients/classes/laptop", []byte(`{"class":"toaster"}`))
			Expect(put.Code).Should(Equal(http.StatusBadRequest))
		})
	})

	Describe("privacy config", func() {
		It("round-trips GET then PUT", func() {
			rec := exec(http.MethodGet, "/api/ui/privacy", nil)
			Expect(rec.Code).Should(Equal(http.StatusOK))
			Expect(jsonBody(rec)).Should(HaveKey("decoy"))

			put := exec(http.MethodPut, "/api/ui/privacy",
				[]byte(`{"decoy":{"enable":true,"queriesPerMinute":6,"replayWeight":3,"listWeight":1,"activeHoursStart":8,"activeHoursEnd":22,"personaProfile":"enterprise","targetQpmPeak":300,"targetQpmTrough":60,"cohortPct":77,"companionPct":42,"sessionCoherence":true,"personaCover":true,"cohortJitterMs":90,"cohortCompanionPct":25},"deviceClass":{"enable":true,"vendorTelemetry":true,"vendorFamilies":["apple","google"],"phantomDevicesPct":25},"ttlJitter":{"enable":true,"percent":15},"ednsPadding":{"enable":true}}`))
			Expect(put.Code).Should(Equal(http.StatusNoContent))

			after := jsonBody(exec(http.MethodGet, "/api/ui/privacy", nil))
			decoy := after["decoy"].(map[string]any)
			Expect(decoy).Should(HaveKeyWithValue("enable", true))
			Expect(decoy).Should(HaveKeyWithValue("replayWeight", BeNumerically("==", 3)))
			Expect(decoy).Should(HaveKeyWithValue("personaProfile", "enterprise"))
			Expect(decoy).Should(HaveKeyWithValue("targetQpmPeak", BeNumerically("==", 300)))
			// structural knobs now round-trip (previously dropped -> zeroed on save)
			Expect(decoy).Should(HaveKeyWithValue("cohortPct", BeNumerically("==", 77)))
			Expect(decoy).Should(HaveKeyWithValue("companionPct", BeNumerically("==", 42)))
			Expect(decoy).Should(HaveKeyWithValue("sessionCoherence", true))
			Expect(decoy).Should(HaveKeyWithValue("personaCover", true))
			Expect(decoy).Should(HaveKeyWithValue("cohortJitterMs", BeNumerically("==", 90)))
			Expect(decoy).Should(HaveKeyWithValue("cohortCompanionPct", BeNumerically("==", 25)))
			dc := after["deviceClass"].(map[string]any)
			Expect(dc).Should(HaveKeyWithValue("vendorTelemetry", true))
			Expect(dc).Should(HaveKeyWithValue("phantomDevicesPct", BeNumerically("==", 25)))
			Expect(dc["vendorFamilies"]).Should(ConsistOf("apple", "google"))
			Expect(after["ttlJitter"]).Should(HaveKeyWithValue("percent", BeNumerically("==", 15)))
		})

		It("rejects an invalid privacy block without persisting", func() {
			put := exec(http.MethodPut, "/api/ui/privacy",
				[]byte(`{"ttlJitter":{"enable":true,"percent":99}}`))
			Expect(put.Code).Should(Equal(http.StatusBadRequest))
		})
	})
})
