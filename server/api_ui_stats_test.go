package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/querylog"

	"github.com/go-chi/chi/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Stats UI API", func() {
	var (
		router *chi.Mux
		hub    *querylog.Hub
		hour1  time.Time
	)

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

	newEntry := func(start time.Time, client, question, responseType string, durationMs int64) *querylog.LogEntry {
		return &querylog.LogEntry{
			Start:        start,
			ClientIP:     "192.168.1.10",
			ClientNames:  []string{client},
			QuestionName: question,
			QuestionType: "A",
			ResponseType: responseType,
			ResponseCode: "NOERROR",
			DurationMs:   durationMs,
			Fingerprint:  model.Fingerprint{Transport: model.TransportDo53UDP},
		}
	}

	BeforeEach(func() {
		ctx, cancelFn := context.WithCancel(context.Background())
		DeferCleanup(cancelFn)

		dbPath := filepath.Join(GinkgoT().TempDir(), "querylog.db")

		writer, err := querylog.NewDatabaseWriter(ctx, config.QueryLogTypeSqlite, dbPath, 7, time.Minute)
		Expect(err).Should(Succeed())

		hour1 = time.Now().Add(-2 * time.Hour).Truncate(time.Hour)

		writer.Write(newEntry(hour1.Add(time.Minute), "laptop", "www.example.com.", "RESOLVED", 30))
		writer.Write(newEntry(hour1.Add(2*time.Minute), "laptop", "ads.tracker.net.", "BLOCKED", 0))
		writer.Write(newEntry(hour1.Add(3*time.Minute), "phone", "www.example.com.", "CACHED", 1))
		Expect(writer.Flush()).Should(Succeed())

		cfg := &config.Config{}
		cfg.QueryLog = config.QueryLog{Type: config.QueryLogTypeSqlite, Target: config.Secret(dbPath)}

		hub = querylog.NewHub()
		router = chi.NewRouter()
		registerStatsUIEndpoints(context.Background(), router, cfg, hub, nil, nil)
	})

	Describe("GET /api/ui/stats/overview", func() {
		It("returns the contract fields", func() {
			rec := exec("/api/ui/stats/overview")

			Expect(rec.Code).Should(Equal(http.StatusOK))
			body := jsonBody(rec)
			Expect(body).Should(HaveKeyWithValue("queries", BeNumerically("==", 3)))
			Expect(body).Should(HaveKeyWithValue("blocked", BeNumerically("==", 1)))
			Expect(body).Should(HaveKeyWithValue("cached", BeNumerically("==", 1)))
			Expect(body).Should(HaveKeyWithValue("clients", BeNumerically("==", 2)))
			Expect(body).Should(HaveKey("avgMs"))
			Expect(body).Should(HaveKey("p95Ms"))
		})

		It("rejects malformed time bounds", func() {
			Expect(exec("/api/ui/stats/overview?from=yesterday").Code).Should(Equal(http.StatusBadRequest))
		})
	})

	Describe("GET /api/ui/stats/buckets", func() {
		It("returns hourly buckets with response-type counts", func() {
			rec := exec("/api/ui/stats/buckets?step=3600")

			Expect(rec.Code).Should(Equal(http.StatusOK))
			buckets := jsonBody(rec)["buckets"].([]any)
			Expect(buckets).Should(HaveLen(1))
			bucket := buckets[0].(map[string]any)
			Expect(bucket).Should(HaveKeyWithValue("ts", BeNumerically("==", hour1.Unix())))
			Expect(bucket["counts"]).Should(HaveKeyWithValue("RESOLVED", BeNumerically("==", 1)))
			Expect(bucket["counts"]).Should(HaveKeyWithValue("BLOCKED", BeNumerically("==", 1)))
		})
	})

	Describe("GET /api/ui/stats/top", func() {
		It("returns ranked domains", func() {
			rec := exec("/api/ui/stats/top?col=domain&n=5")

			Expect(rec.Code).Should(Equal(http.StatusOK))
			items := jsonBody(rec)["items"].([]any)
			first := items[0].(map[string]any)
			Expect(first).Should(HaveKeyWithValue("name", "example.com"))
			Expect(first).Should(HaveKeyWithValue("count", BeNumerically("==", 2)))
		})

		It("rejects an unknown column", func() {
			Expect(exec("/api/ui/stats/top?col=nope").Code).Should(Equal(http.StatusBadRequest))
		})

		It("returns multiple columns in one response when col is comma-separated", func() {
			rec := exec("/api/ui/stats/top?col=domain,blocked&n=5")

			Expect(rec.Code).Should(Equal(http.StatusOK))
			cols := jsonBody(rec)["columns"].(map[string]any)
			Expect(cols).Should(HaveKey("domain"))
			Expect(cols).Should(HaveKey("blocked"))
			Expect(cols["domain"].([]any)[0].(map[string]any)).Should(HaveKeyWithValue("name", "example.com"))
		})
	})

	Describe("GET /api/ui/stats/latency", func() {
		It("returns the percentile fields", func() {
			rec := exec("/api/ui/stats/latency")

			Expect(rec.Code).Should(Equal(http.StatusOK))
			body := jsonBody(rec)
			Expect(body).Should(HaveKey("p50"))
			Expect(body).Should(HaveKey("p90"))
			Expect(body).Should(HaveKey("p95"))
			Expect(body).Should(HaveKey("p99"))
		})
	})

	Describe("GET /api/ui/queries", func() {
		It("returns items and total with filters applied", func() {
			rec := exec("/api/ui/queries?rtype=BLOCKED")

			Expect(rec.Code).Should(Equal(http.StatusOK))
			body := jsonBody(rec)
			Expect(body).Should(HaveKeyWithValue("total", BeNumerically("==", 1)))

			items := body["items"].([]any)
			Expect(items).Should(HaveLen(1))
			item := items[0].(map[string]any)
			Expect(item).Should(HaveKeyWithValue("question", "ads.tracker.net"))
			Expect(item).Should(HaveKeyWithValue("rtype", "BLOCKED"))
			Expect(item).Should(HaveKeyWithValue("client", "192.168.1.10"))
			Expect(item).Should(HaveKeyWithValue("transport", "do53-udp"))
			Expect(item).Should(HaveKeyWithValue("decoy", BeFalse()))
		})
	})

	Describe("GET /api/ui/system", func() {
		It("returns the contract fields", func() {
			rec := exec("/api/ui/system")

			Expect(rec.Code).Should(Equal(http.StatusOK))
			body := jsonBody(rec)
			Expect(body).Should(HaveKey("version"))
			Expect(body).Should(HaveKey("buildTime"))
			Expect(body).Should(HaveKey("uptimeSeconds"))
			Expect(body).Should(HaveKeyWithValue("goroutines", BeNumerically(">", 0)))
			Expect(body).Should(HaveKeyWithValue("heapAllocBytes", BeNumerically(">", 0)))
			Expect(body).Should(HaveKeyWithValue("dbQuerylogBytes", BeNumerically(">", 0)))
			Expect(body).Should(HaveKeyWithValue("dbConfigBytes", BeNumerically("==", 0))) // nil store
			Expect(body).Should(HaveKeyWithValue("queriesTotal", BeNumerically("==", 3)))
		})
	})

	Describe("GET /api/ui/stream", func() {
		It("delivers published queries as SSE events", func() {
			srv := httptest.NewServer(router)
			DeferCleanup(srv.Close)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			DeferCleanup(cancel)

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/ui/stream", nil)
			Expect(err).Should(Succeed())

			resp, err := http.DefaultClient.Do(req)
			Expect(err).Should(Succeed())
			DeferCleanup(resp.Body.Close)

			Expect(resp.Header.Get(contentTypeHeader)).Should(Equal("text/event-stream"))

			// publish until the subscription (established with the request) delivers
			stop := make(chan struct{})
			DeferCleanup(func() { close(stop) })

			go func() {
				for {
					select {
					case <-stop:
						return
					default:
						hub.Publish(newEntry(time.Now(), "laptop", "live.example.com.", "RESOLVED", 3))
						time.Sleep(10 * time.Millisecond)
					}
				}
			}()

			scanner := bufio.NewScanner(resp.Body)

			var event, data string
			for scanner.Scan() {
				line := scanner.Text()
				if after, ok := strings.CutPrefix(line, "event: "); ok {
					event = after
				}

				if after, ok := strings.CutPrefix(line, "data: "); ok {
					data = after

					break
				}
			}

			Expect(event).Should(Equal("query"))

			var item map[string]any
			Expect(json.Unmarshal([]byte(data), &item)).Should(Succeed())
			Expect(item).Should(HaveKeyWithValue("question", "live.example.com"))
			Expect(item).Should(HaveKeyWithValue("rtype", "RESOLVED"))
		})
	})

	When("the query log is not sqlite", func() {
		BeforeEach(func() {
			cfg := &config.Config{}
			cfg.QueryLog = config.QueryLog{Type: config.QueryLogTypeConsole}

			router = chi.NewRouter()
			registerStatsUIEndpoints(context.Background(), router, cfg, nil, nil, nil)
		})

		It("responds 503 on stats, queries and stream", func() {
			for _, path := range []string{
				"/api/ui/stats/overview", "/api/ui/stats/buckets", "/api/ui/stats/top",
				"/api/ui/stats/latency", "/api/ui/queries", "/api/ui/stream",
			} {
				rec := exec(path)
				Expect(rec.Code).Should(Equal(http.StatusServiceUnavailable), path)
				Expect(jsonBody(rec)).Should(HaveKeyWithValue("error", "query log not in sqlite mode"))
			}
		})

		It("still answers /api/ui/system", func() {
			rec := exec("/api/ui/system")
			Expect(rec.Code).Should(Equal(http.StatusOK))
			Expect(jsonBody(rec)).Should(HaveKeyWithValue("queriesTotal", BeNumerically("==", 0)))
		})
	})
})
