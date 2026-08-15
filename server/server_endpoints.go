package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xERR0R/blocky/metrics"
	"github.com/0xERR0R/blocky/resolver"

	"github.com/0xERR0R/blocky/api"
	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/configstore"
	"github.com/0xERR0R/blocky/docs"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/querylog"
	"github.com/0xERR0R/blocky/util"
	"github.com/0xERR0R/blocky/web"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/miekg/dns"
)

const (
	dohMessageLimit    = 512
	contentTypeHeader  = "Content-Type"
	cacheControlHeader = "Cache-Control"
	dnsContentType     = "application/dns-message"
	htmlContentType    = "text/html; charset=UTF-8"
	yamlContentType    = "text/yaml"
	jsonContentType    = "application/json"
)

// createOpenAPIInterfaceImpl wires the OpenAPI handlers to the LIVE resolver
// chain via getter closures instead of freezing the sub-interfaces at build
// time: after a config apply the getters resolve against s.live, so a request
// picks up the new chain immediately and never touches the retired one. Because
// the lookups are per-call, construction can no longer fail on a bad chain —
// hence no error return.
func (s *Server) createOpenAPIInterfaceImpl() api.StrictServerInterface {
	get := func() resolver.ChainedResolver { return s.live.Load().resolver }

	control := func() api.BlockingControl {
		r, _ := resolver.GetFromChainWithType[api.BlockingControl](get())

		return r
	}

	refresher := func() api.ListRefresher {
		r, _ := resolver.GetFromChainWithType[api.ListRefresher](get())

		return r
	}

	cacheControl := func() api.CacheControl {
		r, _ := resolver.GetFromChainWithType[api.CacheControl](get())

		return r
	}

	// Statistics are optional: if no provider is in the chain the getter returns
	// nil and the /api/stats endpoint answers 503 (GetStats nil-guards).
	stats := func() api.StatsProvider {
		r, _ := resolver.GetFromChainWithType[api.StatsProvider](get())

		return r
	}

	return api.NewOpenAPIInterfaceImpl(control, s, refresher, cacheControl, stats)
}

func (s *Server) registerDoHEndpoints(router *chi.Mux, cfg *config.Config) {
	pathDohQuery := cfg.Ports.DOHPath

	router.Get(pathDohQuery, s.dohGetRequestHandler)
	router.Get(pathDohQuery+"/", s.dohGetRequestHandler)
	router.Get(pathDohQuery+"/{clientID}", s.dohGetRequestHandler)
	router.Post(pathDohQuery, s.dohPostRequestHandler)
	router.Post(pathDohQuery+"/", s.dohPostRequestHandler)
	router.Post(pathDohQuery+"/{clientID}", s.dohPostRequestHandler)
}

func (s *Server) dohGetRequestHandler(rw http.ResponseWriter, req *http.Request) {
	dnsParam, ok := req.URL.Query()["dns"]
	if !ok || len(dnsParam[0]) < 1 {
		http.Error(rw, "dns param is missing", http.StatusBadRequest)

		return
	}

	if len(dnsParam[0]) > base64.RawURLEncoding.EncodedLen(dohMessageLimit) {
		http.Error(rw, "URI Too Long", http.StatusRequestURITooLong)

		return
	}

	rawMsg, err := base64.RawURLEncoding.DecodeString(dnsParam[0])
	if err != nil {
		http.Error(rw, "wrong message format", http.StatusBadRequest)

		return
	}

	if len(rawMsg) > dohMessageLimit {
		http.Error(rw, "URI Too Long", http.StatusRequestURITooLong)

		return
	}

	s.processDohMessage(rawMsg, rw, req)
}

func (s *Server) dohPostRequestHandler(rw http.ResponseWriter, req *http.Request) {
	contentType := req.Header.Get(contentTypeHeader)
	if contentType != dnsContentType {
		http.Error(rw, "unsupported content type", http.StatusUnsupportedMediaType)

		return
	}

	rawMsg, err := io.ReadAll(io.LimitReader(req.Body, int64(dohMessageLimit)+1))
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)

		return
	}

	if len(rawMsg) > dohMessageLimit {
		http.Error(rw, "Payload Too Large", http.StatusRequestEntityTooLarge)

		return
	}

	s.processDohMessage(rawMsg, rw, req)
}

func (s *Server) processDohMessage(rawMsg []byte, rw http.ResponseWriter, httpReq *http.Request) {
	msg := new(dns.Msg)
	if err := msg.Unpack(rawMsg); err != nil {
		logger().Error("can't deserialize message: ", err)
		http.Error(rw, err.Error(), http.StatusBadRequest)

		return
	}

	ctx, dnsReq := newRequestFromHTTP(httpReq.Context(), httpReq, msg)

	s.handleReq(ctx, dnsReq, httpMsgWriter{rw})
}

type httpMsgWriter struct {
	rw http.ResponseWriter
}

func (r httpMsgWriter) WriteMsg(msg *dns.Msg) error {
	b, err := msg.Pack()
	if err != nil {
		return fmt.Errorf("failed to pack DNS message for DoH response: %w", err)
	}

	r.rw.Header().Set(contentTypeHeader, dnsContentType)

	// https://www.rfc-editor.org/rfc/rfc8484#section-5.1
	// get the smallest TTL from answer
	r.rw.Header().Set(cacheControlHeader, fmt.Sprintf("max-age=%d", getSmallestTTLFromAnswer(msg)))

	// https://www.rfc-editor.org/rfc/rfc8484#section-4.2.1
	r.rw.WriteHeader(http.StatusOK)

	_, err = r.rw.Write(b)
	if err != nil {
		return fmt.Errorf("failed to write DoH response: %w", err)
	}

	return nil
}

func getSmallestTTLFromAnswer(msg *dns.Msg) uint32 {
	var ttl uint32 = 0
	for _, a := range msg.Answer {
		if a.Header().Ttl < ttl || ttl == 0 {
			ttl = a.Header().Ttl
		}
	}

	return ttl
}

func (s *Server) Query(
	ctx context.Context, serverHost string, clientIP net.IP, question string, qType dns.Type,
) (*model.Response, error) {
	msg := util.NewMsgWithQuestion(question, qType)
	clientID := extractClientIDFromHost(serverHost)

	ctx, req := newRequest(ctx, clientIP, clientID, model.RequestProtocolTCP, msg)

	return s.resolve(ctx, req)
}

func createHTTPRouter(
	ctx context.Context,
	cfg *config.Config, openAPIImpl api.StrictServerInterface, store *configstore.Store, swapper upstreamSwapper,
	qlHub *querylog.Hub, blStats blocklistStatser, classifier clientClassifier,
) (*chi.Mux, io.Closer) {
	router := chi.NewRouter()

	api.RegisterOpenAPIEndpoints(router, openAPIImpl)

	registerConfigUIEndpoints(router, store, swapper)

	registerBlockingUIEndpoints(router, store, blStats)

	registerLocalDNSUIEndpoints(router, store)

	statsAPI := registerStatsUIEndpoints(ctx, router, cfg, qlHub, store, classifier)

	configureDebugHandler(router)

	configureDocsHandler(router)

	configureStaticAssetsHandler(router)

	configureRootHandler(router)

	configureRobotsHandler(router)

	metrics.Start(router, cfg.Prometheus)

	return router, statsAPI
}

func configureDocsHandler(router *chi.Mux) {
	router.Get("/docs/openapi.yaml", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set(contentTypeHeader, yamlContentType)
		_, err := writer.Write([]byte(docs.OpenAPI))
		logAndResponseWithError(err, "can't write OpenAPI definition file: ", writer)
	})

	router.Get("/docs/config.schema.json", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set(contentTypeHeader, jsonContentType)
		_, err := writer.Write(docs.ConfigSchema)
		logAndResponseWithError(err, "can't write config JSON schema file: ", writer)
	})
}

func configureStaticAssetsHandler(router *chi.Mux) {
	assets, err := web.Assets()
	util.FatalOnError("unable to load static asset files", err)

	fs := http.FileServer(http.FS(assets))

	// The assets are baked into the binary, so they only change when the binary
	// does. Without validators the browser re-fetches every module on each page
	// navigation — a page pulls ~20 ES modules plus the uPlot bundle, and over
	// plain HTTP/1.1 (6 connections per host, one permanently held by the SSE
	// stream) that serialises into a visible stall on every tab switch. Tag them
	// with a build-stable ETag and let the browser reuse them.
	etag := `"` + staticAssetsVersion() + `"`

	router.Handle("/static/*", http.StripPrefix("/static/",
		http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			rw.Header().Set("ETag", etag)
			rw.Header().Set("Cache-Control", "public, max-age=3600, must-revalidate")

			// Fast path: the client already has this exact build's assets.
			if match := req.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
				rw.WriteHeader(http.StatusNotModified)

				return
			}

			fs.ServeHTTP(rw, req)
		})))
}

// staticAssetsVersion identifies the embedded asset set. util.Version changes
// on every release build; for dev builds ("undefined") fall back to the start
// time so a restart still invalidates stale caches.
func staticAssetsVersion() string {
	v := util.Version
	if v == "" || v == "undefined" {
		v = strconv.FormatInt(time.Now().Unix(), 10)
	}

	return v
}

func configureRobotsHandler(router *chi.Mux) {
	router.Handle("/robots.txt", http.FileServer(http.FS(web.WebFs)))
}

// uiPages maps SPA shell routes to their page identity (data-page attr) and title.
var uiPages = []struct {
	Route, Page, Title string
}{
	{"/", "dashboard", "Dashboard"},
	{"/live", "live", "Live"},
	{"/queries", "queries", "Queries"},
	{"/noise", "noise", "Noise"},
	{"/clients", "clients", "Clients"},
	{"/upstreams", "upstreams", "Upstreams"},
	{"/blocking", "blocking", "Blocking"},
	{"/localdns", "localdns", "Local DNS"},
	{"/privacy", "privacy", "Privacy"},
	{"/settings", "settings", "Settings"},
	{"/system", "system", "System"},
}

func configureRootHandler(router *chi.Mux) {
	t := template.Must(template.New("shell").Parse(web.ShellTmpl))

	type pageData struct {
		Page    string
		Title   string
		Version string
	}

	for _, p := range uiPages {
		pd := pageData{Page: p.Page, Title: p.Title, Version: util.Version}

		router.Get(p.Route, func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set(contentTypeHeader, htmlContentType)

			err := t.Execute(writer, pd)
			logAndResponseWithError(err, "can't write shell template: ", writer)
		})
	}
}

func logAndResponseWithError(err error, message string, writer http.ResponseWriter) {
	if err != nil {
		log.Log().Error(message, log.EscapeInput(err.Error()))
		http.Error(writer, err.Error(), http.StatusInternalServerError)
	}
}

func configureDebugHandler(router *chi.Mux) {
	router.Mount("/debug", middleware.Profiler())
}
