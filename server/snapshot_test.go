package server

import (
	"testing"
	"time"
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
