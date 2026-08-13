package server

import (
	"fmt"
	"net/http"
	"time"
)

const streamPingInterval = 15 * time.Second

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

	// the HTTP server sets a global WriteTimeout (see newHTTPServer); lift it
	// for this long-lived response. Errors ignored: httptest recorders don't
	// support deadlines and a failed clear only means an eventual reconnect.
	rc := http.NewResponseController(rw)
	_ = rc.SetWriteDeadline(time.Time{})

	flush := func() {
		_ = rc.Flush()
	}

	flush() // commit headers so the client sees the stream open immediately

	ping := time.NewTicker(streamPingInterval)
	defer ping.Stop()

	for {
		select {
		case data := <-events:
			if _, err := fmt.Fprintf(rw, "event: query\ndata: %s\n\n", data); err != nil {
				return
			}

			flush()
		case <-ping.C:
			if _, err := fmt.Fprint(rw, ": ping\n\n"); err != nil {
				return
			}

			flush()
		case <-req.Context().Done():
			return
		}
	}
}
