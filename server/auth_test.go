package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/0xERR0R/blocky/configstore"
)

// Regression: the failed-login window must both prune lapsed entries (rotating
// IPv6 privacy addresses would grow the map without bound) and restart the
// count after a lapsed lockout (one mistype must not re-lock for another
// window).
func TestLoginLimiterWindowReset(t *testing.T) {
	l := newLoginLimiter()

	const ip = "192.0.2.1"

	for range maxLoginFails {
		l.recordFail(ip)
	}

	if !l.locked(ip) {
		t.Fatal("IP should be locked after maxLoginFails failures")
	}

	// lapse the window, and plant a stale rotating-address entry to prune
	l.mu.Lock()
	e := l.fails[ip]
	e.until = time.Now().Add(-time.Second)
	l.fails[ip] = e
	l.fails["2001:db8::1"] = failEntry{count: 1, until: time.Now().Add(-time.Hour)}
	l.mu.Unlock()

	if l.locked(ip) {
		t.Fatal("lockout should lapse with the window")
	}

	// a single failure after the lapsed window must NOT re-lock
	l.recordFail(ip)

	if l.locked(ip) {
		t.Fatal("one failure after a lapsed window must not re-lock")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if got := l.fails[ip].count; got != 1 {
		t.Fatalf("count after lapsed window = %d, want 1", got)
	}

	if _, ok := l.fails["2001:db8::1"]; ok {
		t.Fatal("lapsed entries must be pruned from the map")
	}
}

// Regression: the loopback auth exemption must never apply to a connection
// that arrived through a PROXY-protocol listener — there RemoteAddr is
// sender-claimed, so a forged `PROXY TCP4 127.0.0.1 …` line would grant admin.
// Also covers the legacy mutating /api routes now being session-gated.
func TestSessionGateProxyProtoAndLegacyRoutes(t *testing.T) {
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	var reached bool

	gate := newSessionGate(store, "/dns-query", "/metrics")(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	serve := func(path, remoteAddr string, viaProxy bool) *httptest.ResponseRecorder {
		t.Helper()

		reached = false
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = remoteAddr

		if viaProxy {
			req = req.WithContext(context.WithValue(req.Context(), viaProxyProtoKey{}, true))
		}

		rec := httptest.NewRecorder()
		gate.ServeHTTP(rec, req)

		return rec
	}

	// direct loopback stays exempt (HDMI dashboard poller)
	serve("/settings", "127.0.0.1:4711", false)

	if !reached {
		t.Fatal("direct loopback should be exempt from auth")
	}

	// forged loopback via PROXY protocol must NOT be exempt
	rec := serve("/settings", "127.0.0.1:4711", true)

	if reached {
		t.Fatal("PROXY-protocol loopback must not bypass auth")
	}

	if rec.Code != http.StatusFound {
		t.Fatalf("want login redirect (302), got %d", rec.Code)
	}

	// legacy mutating control route requires a session
	rec = serve("/api/blocking/disable", "192.168.1.50:4711", false)

	if reached {
		t.Fatal("legacy mutating /api route must require a session")
	}

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for ungated mutation, got %d", rec.Code)
	}

	// read-only legacy route stays open for cookieless integrations
	serve("/api/blocking/status", "192.168.1.50:4711", false)

	if !reached {
		t.Fatal("read-only legacy /api route should stay ungated")
	}
}

// Regression: a SameSite=Lax session cookie still rides on top-level cross-site
// GET navigations, and the legacy control routes mutate on GET — a link to
// /api/blocking/disable must not disable blocking with the victim's cookie.
// Mutations require a same-origin Origin/Referer; absent both fails closed.
func TestSessionGateCSRFGuard(t *testing.T) {
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	secret, err := store.SessionSecret()
	if err != nil {
		t.Fatalf("session secret: %v", err)
	}

	cookie := signSession(secret, time.Now().Add(time.Hour).Unix())

	var reached bool

	gate := newSessionGate(store, "/dns-query", "/metrics")(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	serve := func(method, path string, hdr map[string]string) *httptest.ResponseRecorder {
		t.Helper()

		reached = false
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "192.168.1.50:4711"
		req.Host = "pi.hole"
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})

		for k, v := range hdr {
			req.Header.Set(k, v)
		}

		rec := httptest.NewRecorder()
		gate.ServeHTTP(rec, req)

		return rec
	}

	// cross-site top-level GET navigation: cookie present, no same-origin proof
	rec := serve(http.MethodGet, "/api/blocking/disable", nil)
	if reached || rec.Code != http.StatusForbidden {
		t.Fatalf("mutating GET without Origin/Referer must be 403, got %d (reached=%v)", rec.Code, reached)
	}

	rec = serve(http.MethodGet, "/api/blocking/disable", map[string]string{"Referer": "https://evil.example/"})
	if reached || rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin mutating GET must be 403, got %d (reached=%v)", rec.Code, reached)
	}

	// same-origin mutation passes
	serve(http.MethodGet, "/api/blocking/disable", map[string]string{"Referer": "http://pi.hole/settings"})

	if !reached {
		t.Fatal("same-origin mutating GET should pass the CSRF guard")
	}

	serve(http.MethodPost, "/api/ui/config", map[string]string{"Origin": "http://pi.hole"})

	if !reached {
		t.Fatal("same-origin POST should pass the CSRF guard")
	}

	// non-mutating page load needs no source header (bookmarks, direct entry)
	serve(http.MethodGet, "/settings", nil)

	if !reached {
		t.Fatal("read-only GET must not require Origin/Referer")
	}
}

// Regression: the public metrics exemption must follow the CONFIGURED scrape
// path — a custom path must stay scrapeable off-box, and the literal /metrics
// must be gated once the handler moved away from it.
func TestSessionGateMetricsPathFollowsConfig(t *testing.T) {
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	var reached bool

	gate := newSessionGate(store, "", "/custom-metrics")(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	serve := func(path string) {
		t.Helper()

		reached = false
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "192.168.1.50:4711"
		gate.ServeHTTP(httptest.NewRecorder(), req)
	}

	serve("/custom-metrics")

	if !reached {
		t.Fatal("configured metrics path must be public")
	}

	serve("/metrics")

	if reached {
		t.Fatal("literal /metrics must be gated when the scrape path is custom")
	}
}
