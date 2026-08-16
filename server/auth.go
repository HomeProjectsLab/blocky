package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xERR0R/blocky/configstore"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/util"
	"github.com/go-chi/chi/v5"
	"github.com/sirupsen/logrus"
)

// authLog is the component-prefixed logger for auth events. Auth logs record the
// EVENT + client IP + outcome — never the password, hash, or session secret.
func authLog() *logrus.Entry { return log.PrefixedLog("auth") }

const (
	sessionCookie = "blocky_session"
	sessionTTL    = 12 * time.Hour
	// re-issue the cookie once less than half its life remains (sliding refresh)
	sessionRefresh = 6 * time.Hour
	minPasswordLen = 8

	// per-IP failed-login lockout
	maxLoginFails = 5
	lockoutWindow = 15 * time.Minute
)

// --- signed session cookie (HMAC-SHA256 over "uid|expiry") ---

func signSession(secret []byte, expiry int64) string {
	payload := "1|" + strconv.FormatInt(expiry, 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))

	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifySession returns whether the cookie is authentic and unexpired, plus its
// expiry (for sliding refresh). Comparison is constant-time via hmac.Equal.
func verifySession(secret []byte, cookie string) (bool, int64) {
	parts := strings.SplitN(cookie, ".", 2)
	if len(parts) != 2 {
		return false, 0
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false, 0
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false, 0
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)

	if !hmac.Equal(sig, mac.Sum(nil)) {
		return false, 0
	}

	fields := strings.SplitN(string(payload), "|", 2)
	if len(fields) != 2 {
		return false, 0
	}

	expiry, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return false, expiry
	}

	return true, expiry
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, secret []byte) {
	exp := time.Now().Add(sessionTTL)

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    signSession(secret, exp.Unix()),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		Expires:  exp,
		MaxAge:   int(sessionTTL / time.Second),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   -1,
	})
}

// --- the session gate ---

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// isPublic lists paths the gate never protects: the login page and its assets,
// the auth endpoints themselves, the DoH resolver path and the metrics scrape.
func isPublic(path, dohPath string) bool {
	switch {
	case path == "/login",
		strings.HasPrefix(path, "/static/"),
		strings.HasPrefix(path, "/api/ui/auth/"),
		path == "/metrics":
		return true
	case dohPath != "" && (path == dohPath || strings.HasPrefix(path, dohPath+"/")):
		return true
	}

	return false
}

// legacyMutatingAPI lists the legacy OpenAPI control routes that change server
// state. They require a session (a drive-by cross-origin GET must not disable
// blocking); the read-only legacy routes stay open for cookieless integrations.
func legacyMutatingAPI(path string) bool {
	switch path {
	case "/api/blocking/enable", "/api/blocking/disable",
		"/api/cache/flush", "/api/lists/refresh":
		return true
	}

	return false
}

// newSessionGate protects the web UI (the SPA page routes and /api/ui/*) and
// the legacy mutating /api control routes. The read-only legacy routes are left
// ungated on purpose: Grafana and friends call them cross-origin and cookieless
// (see newCORSMiddleware). store nil makes the gate a no-op (tests /
// YAML-import mode).
func newSessionGate(store *configstore.Store, dohPath string) httpMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The tty dashboard client polls the API from localhost with no
			// cookie — exempt loopback or that HDMI panel goes dark. NEVER for
			// a PROXY-protocol connection: there RemoteAddr is sender-claimed,
			// and a forged `PROXY TCP4 127.0.0.1 …` line from any LAN host
			// would otherwise grant full admin without a password.
			if isLoopback(r.RemoteAddr) && !viaProxyProtocol(r) {
				next.ServeHTTP(w, r)

				return
			}

			path := r.URL.Path

			if isPublic(path, dohPath) {
				next.ServeHTTP(w, r)

				return
			}

			// Only the web UI and the legacy mutating control routes are gated:
			// any other /api/ route is a read-only legacy route — never gated.
			if strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/api/ui/") &&
				!legacyMutatingAPI(path) {
				next.ServeHTTP(w, r)

				return
			}

			if store == nil {
				next.ServeHTTP(w, r)

				return
			}

			// Fail closed: without a usable 32-byte secret every cookie check
			// would run against a nil/short HMAC key, which an attacker can
			// forge offline. Reject before verifySession ever runs.
			secret, err := store.SessionSecret()
			if err != nil || len(secret) != sha256.Size {
				authLog().WithError(err).Error("session secret unavailable, rejecting request")
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authentication unavailable"})

				return
			}

			if c, err := r.Cookie(sessionCookie); err == nil {
				if ok, exp := verifySession(secret, c.Value); ok {
					if time.Until(time.Unix(exp, 0)) < sessionRefresh {
						setSessionCookie(w, r, secret)
					}

					next.ServeHTTP(w, r)

					return
				}
			}

			// Unauthenticated (or not yet configured): API callers get 401 JSON,
			// browsers get bounced to the login/setup page.
			if strings.HasPrefix(path, "/api/") {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})

				return
			}

			http.Redirect(w, r, "/login", http.StatusFound)
		})
	}
}

// --- auth endpoints ---

// loginLimiter is an in-memory per-IP failed-login lockout.
// ponytail: process-local map, resets on restart — fine for a single-box home
// resolver; move to the store if you ever run more than one instance.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]failEntry
}

type failEntry struct {
	count int
	until time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{fails: map[string]failEntry{}}
}

func (l *loginLimiter) locked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	e := l.fails[ip]

	return e.count >= maxLoginFails && time.Now().Before(e.until)
}

func (l *loginLimiter) recordFail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()

	// Prune every lapsed-window entry: rotating (IPv6 privacy) addresses would
	// otherwise grow the map without bound, and a lapsed entry keeping its old
	// count would re-lock after a single mistype post-lockout. Deleting it means
	// this failure starts a fresh window at count 1.
	for k, e := range l.fails {
		if now.After(e.until) {
			delete(l.fails, k)
		}
	}

	e := l.fails[ip]
	e.count++
	e.until = now.Add(lockoutWindow)
	l.fails[ip] = e

	if e.count == maxLoginFails {
		authLog().WithField("client_ip", util.Obfuscate(ip)).
			WithField("window", lockoutWindow.String()).
			Warn("login lockout triggered for IP")
	}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.fails, ip)
}

type authAPI struct {
	store   *configstore.Store
	limiter *loginLimiter
}

func registerAuthEndpoints(router *chi.Mux, store *configstore.Store) {
	a := &authAPI{store: store, limiter: newLoginLimiter()}

	router.Route("/api/ui/auth", func(r chi.Router) {
		r.Post("/setup", a.setup)
		r.Post("/login", a.login)
		r.Post("/logout", a.logout)
		r.Get("/status", a.status)
	})
}

func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

// decodePassword reads {"password": "..."} from the body.
func decodePassword(r *http.Request) (string, bool) {
	var body struct {
		Password string `json:"password"`
	}

	data, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		return "", false
	}

	if err := json.Unmarshal(data, &body); err != nil {
		return "", false
	}

	return body.Password, true
}

func (a *authAPI) setup(w http.ResponseWriter, r *http.Request) {
	if a.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "config store not available"})

		return
	}

	if a.store.IsAuthConfigured() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already configured"})

		return
	}

	pw, ok := decodePassword(r)
	if !ok || len(pw) < minPasswordLen {
		writeJSON(w, http.StatusBadRequest,
			map[string]string{"error": "password must be at least 8 characters"})

		return
	}

	if err := a.store.SetPassword(pw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})

		return
	}

	secret, err := a.store.SessionSecret()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "can't create session"})

		return
	}

	setSessionCookie(w, r, secret)
	authLog().WithField("client_ip", util.Obfuscate(requestIP(r))).
		Info("first-run setup completed: admin password set")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *authAPI) login(w http.ResponseWriter, r *http.Request) {
	if a.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "config store not available"})

		return
	}

	ip := requestIP(r)
	obfIP := util.Obfuscate(ip)

	if a.limiter.locked(ip) {
		authLog().WithField("client_ip", obfIP).Warn("login rejected: IP is locked out")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many attempts, try again later"})

		return
	}

	pw, ok := decodePassword(r)
	if !ok || !a.store.VerifyPassword(pw) {
		a.limiter.recordFail(ip)
		authLog().WithField("client_ip", obfIP).Warn("login failed: invalid password")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid password"})

		return
	}

	secret, err := a.store.SessionSecret()
	if err != nil {
		authLog().WithError(err).Error("session secret unavailable, can't issue session")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "can't create session"})

		return
	}

	a.limiter.reset(ip)
	setSessionCookie(w, r, secret)
	authLog().WithField("client_ip", obfIP).Info("login succeeded")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *authAPI) logout(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w, r)
	authLog().WithField("client_ip", util.Obfuscate(requestIP(r))).Info("logout")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *authAPI) status(w http.ResponseWriter, r *http.Request) {
	configured := a.store != nil && a.store.IsAuthConfigured()

	authenticated := false
	if configured {
		if c, err := r.Cookie(sessionCookie); err == nil {
			// fail closed: on a secret read error the caller stays unauthenticated
			if secret, serr := a.store.SessionSecret(); serr == nil {
				authenticated, _ = verifySession(secret, c.Value)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]bool{
		"configured":    configured,
		"authenticated": authenticated,
	})
}
