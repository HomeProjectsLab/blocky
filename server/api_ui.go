package server

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/0xERR0R/blocky/configstore"

	"github.com/go-chi/chi/v5"
)

// registerConfigUIEndpoints mounts the raw-config management API under /api/ui.
// store may be nil (e.g. tests or YAML import mode): all endpoints then respond 503.
// swapper may be nil: upstream entry updates then always report needsApply.
func registerConfigUIEndpoints(router *chi.Mux, store *configstore.Store, swapper upstreamSwapper) {
	u := &uiAPI{store: store, swapper: swapper}

	router.Route("/api/ui/config", func(r chi.Router) {
		r.Get("/raw", u.getRaw)
		r.Put("/raw", u.putRaw)
		r.Post("/validate", u.validate)
		r.Post("/apply", u.apply)
		r.Get("/status", u.status)
		r.Get("/export", u.exportConfig)
		r.Post("/import", u.importConfig)
	})

	router.Route("/api/ui/upstreams", func(r chi.Router) {
		r.Get("/", u.getUpstreams)
		r.Put("/groups/{name}", u.putUpstreamGroup)
		r.Delete("/groups/{name}", u.deleteUpstreamGroup)
		r.Put("/groups/{name}/entries", u.putUpstreamEntries)
		r.Get("/conditional", u.getConditional)
		r.Put("/conditional", u.putConditional)
	})
}

type uiAPI struct {
	store   *configstore.Store
	swapper upstreamSwapper
}

// maxRawConfigBytes caps a raw-YAML body (putRaw/validate). Real configs are
// KBs; loopback is auth-exempt, so an unbounded ReadAll would let one request
// OOM the resolver on the 1GB Pi.
const maxRawConfigBytes = 1 << 20 // 1 MiB

func writeJSON(rw http.ResponseWriter, status int, body any) {
	rw.Header().Set(contentTypeHeader, jsonContentType)
	rw.WriteHeader(status)

	err := json.NewEncoder(rw).Encode(body)
	if err != nil {
		logger().Error("can't write JSON response: ", err)
	}
}

// storeUnavailable handles the nil-store case; returns true if the request was already answered.
func (u *uiAPI) storeUnavailable(rw http.ResponseWriter) bool {
	if u.store == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "config store not available"})

		return true
	}

	return false
}

func (u *uiAPI) getRaw(rw http.ResponseWriter, _ *http.Request) {
	if u.storeUnavailable(rw) {
		return
	}

	raw, err := u.store.RawYAML()
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]string{"error": err.Error()})

		return
	}

	rw.Header().Set(contentTypeHeader, yamlContentType)
	_, err = rw.Write([]byte(raw))
	logAndResponseWithError(err, "can't write raw config: ", rw)
}

func (u *uiAPI) putRaw(rw http.ResponseWriter, req *http.Request) {
	if u.storeUnavailable(rw) {
		return
	}

	req.Body = http.MaxBytesReader(rw, req.Body, maxRawConfigBytes)

	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})

		return
	}

	if err := u.store.SetRawYAML(string(body)); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})

		return
	}

	rw.WriteHeader(http.StatusNoContent)
}

func (u *uiAPI) validate(rw http.ResponseWriter, req *http.Request) {
	if u.storeUnavailable(rw) {
		return
	}

	req.Body = http.MaxBytesReader(rw, req.Body, maxRawConfigBytes)

	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})

		return
	}

	data := string(body)
	if len(data) == 0 {
		// empty body = validate the stored blob
		data, err = u.store.RawYAML()
		if err != nil {
			writeJSON(rw, http.StatusInternalServerError, map[string]string{"error": err.Error()})

			return
		}
	}

	if err := u.store.ValidateRaw(data); err != nil {
		writeJSON(rw, http.StatusOK, map[string]any{"valid": false, "error": err.Error()})

		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{"valid": true})
}

func (u *uiAPI) apply(rw http.ResponseWriter, _ *http.Request) {
	if u.storeUnavailable(rw) {
		return
	}

	u.store.RequestApply()

	writeJSON(rw, http.StatusAccepted, map[string]string{"status": "applying"})
}

func (u *uiAPI) status(rw http.ResponseWriter, _ *http.Request) {
	if u.storeUnavailable(rw) {
		return
	}

	dirty, lastApplied, updatedAt, err := u.store.Status()
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]string{"error": err.Error()})

		return
	}

	var lastAppliedJSON any
	if !lastApplied.IsZero() {
		lastAppliedJSON = lastApplied.Format(time.RFC3339)
	}

	writeJSON(rw, http.StatusOK, map[string]any{
		"dirty":       dirty,
		"lastApplied": lastAppliedJSON,
		"updatedAt":   updatedAt.Format(time.RFC3339),
	})
}
