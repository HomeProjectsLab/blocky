package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/go-chi/chi/v5"
	"github.com/pires/go-proxyproto"
	"github.com/rs/cors"
)

type httpServer struct {
	inner http.Server

	name string
}

func newHTTPServer(name string, handler http.Handler, cfg *config.Config) *httpServer {
	var (
		writeTimeout      = cfg.Blocking.Loading.Downloads.WriteTimeout
		readTimeout       = cfg.Blocking.Loading.Downloads.ReadTimeout
		readHeaderTimeout = cfg.Blocking.Loading.Downloads.ReadHeaderTimeout
	)

	return &httpServer{
		inner: http.Server{
			ReadTimeout:       time.Duration(readTimeout),
			ReadHeaderTimeout: time.Duration(readHeaderTimeout),
			WriteTimeout:      time.Duration(writeTimeout),
			Handler:           withCommonMiddleware(handler),
			ConnContext:       connContext,
		},

		name: name,
	}
}

// viaProxyProtoKey marks connections accepted through a PROXY-protocol
// listener. Their RemoteAddr is whatever the sender wrote into the PROXY
// header, so it must never satisfy an address-based trust decision (see the
// loopback exemption in newSessionGate).
type viaProxyProtoKey struct{}

func connContext(ctx context.Context, c net.Conn) context.Context {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}

	if _, ok := c.(*proxyproto.Conn); ok {
		return context.WithValue(ctx, viaProxyProtoKey{}, true)
	}

	return ctx
}

// viaProxyProtocol reports whether the request arrived over a PROXY-protocol
// wrapped connection, i.e. whether its RemoteAddr is sender-claimed.
func viaProxyProtocol(r *http.Request) bool {
	via, _ := r.Context().Value(viaProxyProtoKey{}).(bool)

	return via
}

func (s *httpServer) String() string {
	return s.name
}

func (s *httpServer) Serve(l net.Listener) error {
	// The server is closed synchronously by Server.Stop (inner.Close), which
	// guarantees the listener socket is released before Stop returns — a full
	// rebuild can then rebind the same ports without racing an async shutdown
	// (EADDRINUSE → supervisor fatal-exit). A config apply swaps the resolver
	// bundle without ever touching this server, keeping :80/:443 hot.
	if err := s.inner.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP server '%s' failed to serve: %w", s.name, err)
	}

	return nil
}

func withCommonMiddleware(inner http.Handler) *chi.Mux {
	// Middleware must be defined before routes, so
	// create a new router and mount the inner handler
	mux := chi.NewMux()

	mux.Use(
		secureHeadersMiddleware,
		newCORSMiddleware(),
	)

	mux.Mount("/", inner)

	return mux
}

type httpMiddleware = func(http.Handler) http.Handler

func secureHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// These need no TLS — set them on every response so the UI is
		// clickjacking/sniffing-protected over plain HTTP too.
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=63072000")
			w.Header().Set("x-xss-protection", "1; mode=block")
		}

		next.ServeHTTP(w, r)
	})
}

func newCORSMiddleware() httpMiddleware {
	const corsMaxAge = 5 * time.Minute

	options := cors.Options{
		AllowCredentials: true,
		// Allow all request headers: web UIs send tool-specific headers and a
		// disallowed header makes the preflight fail. The API attaches no
		// security semantics to request headers, and rs/cors answers a
		// wildcard by echoing the requested headers, which is spec-compliant
		// also for 'Authorization'.
		AllowedHeaders: []string{"*"},
		AllowedMethods: []string{"GET", "POST"},
		// Same-origin only. The previous wildcard origin (plus
		// AllowPrivateNetwork) let any website's JS drive the API cross-origin
		// as a drive-by (PNA preflight waived). Browsers are the only CORS
		// enforcers, so non-browser integrations (Grafana backend datasources,
		// curl, scripts) are unaffected by this policy.
		AllowOriginVaryRequestFunc: func(r *http.Request, origin string) (bool, []string) {
			u, err := url.Parse(origin)

			return err == nil && u.Host == r.Host, nil
		},
		ExposedHeaders: []string{"Link"},
		MaxAge:         int(corsMaxAge.Seconds()),
	}

	return cors.New(options).Handler
}
