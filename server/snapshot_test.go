package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/0xERR0R/blocky/querylog"
)

// TestIsDefaultWindow checks the tolerant match that decides whether a request
// is served from the preheated snapshot: the browser stamps a fresh to=now /
// from=now-24h on every call, so equality never holds — only the ~24h-ending-now
// shape (within slop) may match, and everything else must fall through to live.
func TestIsDefaultWindow(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name     string
		from, to time.Time
		want     bool
	}{
		{"exact default", now.Add(-defaultWindow), now, true},
		{"drifted a few seconds", now.Add(-defaultWindow + 3*time.Second), now.Add(-2 * time.Second), true},
		{"7d range", now.Add(-7 * 24 * time.Hour), now, false},
		{"1h range", now.Add(-time.Hour), now, false},
		{"historical 24h window (to far in past)", now.Add(-49 * time.Hour), now.Add(-25 * time.Hour), false},
		{"width right but to in future", now.Add(defaultWindow), now.Add(2 * defaultWindow), false},
	}

	for _, c := range cases {
		if got := isDefaultWindow(c.from, c.to); got != c.want {
			t.Errorf("%s: isDefaultWindow = %v, want %v", c.name, got, c.want)
		}
	}
}

// Regression (the web-UI-blocking fix): once populated, the default window is
// ALWAYS served from the snapshot — even arbitrarily stale — so a slow/failing
// refresh never strands a tab on an unbounded live read. Only a never-populated
// snapshot (genuine cold boot) or a non-default window falls through.
func TestSnapshotServesStale(t *testing.T) {
	now := time.Now()
	from, to := now.Add(-defaultWindow), now

	// Cold boot: never populated -> not served (handler falls through to a
	// BOUNDED live read).
	if _, ok := (&statsSnapshot{}).getOverview(from, to); ok {
		t.Fatal("never-populated snapshot must not be served")
	}

	// Populated, no freshness timestamp at all: still served for the default
	// window — a stale number beats a hung tab.
	snap := &statsSnapshot{populated: true, overview: &querylog.Overview{Queries: 7}}

	ov, ok := snap.getOverview(from, to)
	if !ok || ov == nil || ov.Queries != 7 {
		t.Fatalf("populated snapshot must serve stale: ok=%v ov=%v", ok, ov)
	}

	// A non-default (custom) window never comes from the snapshot, populated or not.
	if _, ok := snap.getOverview(now.Add(-7*24*time.Hour), now); ok {
		t.Fatal("non-default window must fall through, not serve the snapshot")
	}
}

// Regression: a request-path fall-through read is deadline-bounded — a read that
// outlasts the timeout returns context.DeadlineExceeded fast (handler answers
// 503) instead of hanging; a fast read returns its value.
func TestBoundedReadDeadline(t *testing.T) {
	if v, err := boundedRead(time.Second, func() (int, error) { return 42, nil }); err != nil || v != 42 {
		t.Fatalf("fast read: v=%d err=%v", v, err)
	}

	_, err := boundedRead(10*time.Millisecond, func() (int, error) {
		time.Sleep(time.Second)

		return 1, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow read: want DeadlineExceeded, got %v", err)
	}
}
