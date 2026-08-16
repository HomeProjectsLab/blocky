package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/0xERR0R/blocky/log"

	"github.com/sirupsen/logrus"
)

// logsStream serves the live application log as Server-Sent Events: one "log"
// event per captured logrus entry (JSON in the log.LogLine shape), plus a
// ": ping" comment every 15s. Mirrors stream() exactly; the source is the
// always-present log.Console singleton, so there's no 503 branch. The client
// filters by level — the stream is a pure mirror.
func (s *statsAPI) logsStream(rw http.ResponseWriter, req *http.Request) {
	events, unsubscribe := log.Console.Subscribe()
	defer unsubscribe()

	rw.Header().Set(contentTypeHeader, "text/event-stream")
	rw.Header().Set(cacheControlHeader, "no-cache")
	rw.WriteHeader(http.StatusOK)

	// the HTTP server sets a global WriteTimeout (see newHTTPServer); replace
	// it with a finite rolling deadline refreshed before each write (mirrors
	// stream()), so a half-open peer fails fast instead of leaking the
	// goroutine + subscription. Errors ignored: httptest recorders don't
	// support deadlines.
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

			if _, err := fmt.Fprintf(rw, "event: log\ndata: %s\n\n", data); err != nil {
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

// logsRecent writes the console ring as a JSON array of log.LogLine objects
// (oldest first). An optional ?level= (logrus level name) drops entries below
// that level; an absent or unparseable value returns everything.
func (s *statsAPI) logsRecent(rw http.ResponseWriter, req *http.Request) {
	var min *logrus.Level
	if lvl, err := logrus.ParseLevel(req.URL.Query().Get("level")); err == nil {
		min = &lvl
	}

	lines := log.Console.Recent(min)

	rw.Header().Set(contentTypeHeader, jsonContentType)
	_, _ = rw.Write([]byte("["))

	for i, l := range lines {
		if i > 0 {
			_, _ = rw.Write([]byte(","))
		}

		_, _ = rw.Write(l)
	}

	_, _ = rw.Write([]byte("]"))
}
