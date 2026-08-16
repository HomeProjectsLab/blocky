package server

import (
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
)

// pathParam returns the named chi path parameter with percent-escapes decoded.
// chi matches on the ESCAPED path and never unescapes params, so an identifier
// like the CIDR client "192.168.1.0/24" arrives as "192.168.1.0%2F24" — stored
// literally it never parses as CIDR and silently never matches. Every
// identifier path-param read must go through here. A malformed escape falls
// back to the raw value (it's an identifier, not a guard: it just won't match).
func pathParam(req *http.Request, name string) string {
	raw := chi.URLParam(req, name)
	if v, err := url.PathUnescape(raw); err == nil {
		return v
	}

	return raw
}
