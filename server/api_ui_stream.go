package server

import (
	"fmt"
	"net/http"
	"time"
)

const streamPingInterval = 15 * time.Second

// streamWriteDeadline is the rolling per-write deadline on an SSE response: a
// half-open client fails a write within ~3 ping intervals instead of leaking
// the goroutine + subscription for the kernel's 15-20min TCP timeout (which a
// cleared deadline allowed).
const streamWriteDeadline = 3 * streamPingInterval

// stream serves the live query feed as Server-Sent Events: one "query" event
// per resolved query (JSON in the /api/ui/queries item shape), plus a ": ping"
// comment every 15s so proxies and clients see a live connection.
func (s *statsAPI) stream(rw http.ResponseWriter, req *http.Request) {
	if s.hub == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "query log not in sqlite mode"})

		return
	}

	events, unsubscribe := s.hub.Subscribe()
	defer unsubscribe()

	rw.Header().Set(contentTypeHeader, "text/event-stream")
	rw.Header().Set(cacheControlHeader, "no-cache")
	rw.WriteHeader(http.StatusOK)

	// the HTTP server sets a global WriteTimeout (see newHTTPServer); replace
	// it with a finite rolling deadline refreshed before each write, so this
	// long-lived response never times out while alive but a dead peer still
	// fails fast. Errors ignored: httptest recorders don't support deadlines.
	rc := http.NewResponseController(rw)

	extendDeadline := func() {
		_ = rc.SetWriteDeadline(time.Now().Add(streamWriteDeadline))
	}
	extendDeadline()

	flush := func() {
		_ = rc.Flush()
	}

	flush() // commit headers so the client sees the stream open immediately

	ping := time.NewTicker(streamPingInterval)
	defer ping.Stop()

	for {
		select {
		case data := <-events:
			extendDeadline()

			if _, err := fmt.Fprintf(rw, "event: query\ndata: %s\n\n", data); err != nil {
				return
			}

			flush()
		case <-ping.C:
			extendDeadline()

			if _, err := fmt.Fprint(rw, ": ping\n\n"); err != nil {
				return
			}

			flush()
		case <-req.Context().Done():
			return
		}
	}
}
