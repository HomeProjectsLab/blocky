package tui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A silently-dead stream (no data, no FIN/RST) must make Stream return within
// the stall timeout so streamLoop can reconnect, instead of blocking for the
// ~5min kernel keepalive.
func TestStreamStallWatchdogReconnects(t *testing.T) {
	hang := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: query\ndata: {\"question\":\"x\"}\n\n")
		w.(http.Flusher).Flush()
		<-hang // stall: no pings, no close
	}))

	// LIFO: close(hang) must run BEFORE srv.Close(), which waits for the handler
	defer srv.Close()
	defer close(hang)

	old := streamStallTimeout
	streamStallTimeout = 200 * time.Millisecond

	defer func() { streamStallTimeout = old }()

	done := make(chan struct{})
	got := 0

	go func() {
		_ = NewClient(srv.URL).Stream(func(QueryItem) { got++ })
		close(done)
	}()

	select {
	case <-done:
		if got != 1 {
			t.Fatalf("expected 1 event before stall, got %d", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stream did not return on a stalled connection")
	}
}
