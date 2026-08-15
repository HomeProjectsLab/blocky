package querylog

import (
	"testing"
	"time"
)

func TestQPSCounter(t *testing.T) {
	var q qpsCounter
	base := int64(1_000_000)

	// 10 queries in each of 10 consecutive seconds
	for s := base; s < base+10; s++ {
		for i := 0; i < 10; i++ {
			q.incr(s)
		}
	}

	if r := q.rate(base+9, 10); r != 10 { // 100 queries / 10s
		t.Errorf("10s rate = %v, want 10", r)
	}

	if r := q.rate(base+9, 5); r != 10 { // 50 / 5s
		t.Errorf("5s rate = %v, want 10", r)
	}

	if r := q.rate(base+100, 10); r != 0 { // window past all data
		t.Errorf("empty-window rate = %v, want 0", r)
	}

	if r := q.rate(base+9, 0); r != 0 { // degenerate window
		t.Errorf("zero window = %v, want 0", r)
	}

	// window longer than the ring is clamped to 3600 (no double-count wraparound)
	if r := q.rate(base+9, 100000); r != 100.0/3600.0 {
		t.Errorf("over-long window = %v, want %v", r, 100.0/3600.0)
	}

	// stale bucket: reusing an index an hour later must drop the old second
	var q2 qpsCounter
	q2.incr(base)
	q2.incr(base + 3600) // same bucket index, one hour later
	if r := q2.rate(base+3600, 1); r != 1 {
		t.Errorf("after stale reset, 1s rate = %v, want 1", r)
	}
	if r := q2.rate(base, 1); r != 0 {
		t.Errorf("old second should be gone, got %v", r)
	}
}

func TestHubQPSCountsEvenWithoutSubscribers(t *testing.T) {
	h := NewHub()
	// no Subscribe() call — Publish still counts (increment is before the
	// no-subscriber early-return)
	for i := 0; i < 7; i++ {
		h.Publish(&LogEntry{Start: time.Now()})
	}

	if r := h.QPS(2 * time.Second); r <= 0 {
		t.Errorf("QPS after 7 publishes = %v, want > 0", r)
	}

	if r := (*Hub)(nil).QPS(time.Second); r != 0 {
		t.Errorf("nil hub QPS = %v, want 0", r)
	}
}
