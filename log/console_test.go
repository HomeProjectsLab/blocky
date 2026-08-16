package log

import (
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// entry builds a bare *logrus.Entry the hook can Fire, without going through a
// logger (so the test never depends on the global logger's level/output).
func entry(msg string) *logrus.Entry {
	return &logrus.Entry{
		Logger:  logrus.New(),
		Time:    time.Now(),
		Level:   logrus.InfoLevel,
		Message: msg,
		Data:    logrus.Fields{},
	}
}

// (a) Fire more than the ring holds → Recent returns exactly logRingSize, newest.
func TestLogHubRingBounded(t *testing.T) {
	h := &LogHub{subs: map[chan []byte]struct{}{}}

	for i := 0; i < logRingSize+10; i++ {
		_ = h.Fire(entry("line"))
	}

	got := h.Recent(nil)
	if len(got) != logRingSize {
		t.Fatalf("Recent() = %d lines, want %d", len(got), logRingSize)
	}
}

// (b) Subscribe without draining, then Fire far more than the subscriber buffer:
// Fire must never block (drop-on-full). A hang here fails the test via timeout.
func TestLogHubFireNeverBlocks(t *testing.T) {
	h := &LogHub{subs: map[chan []byte]struct{}{}}

	_, unsub := h.Subscribe() // never drained
	defer unsub()

	done := make(chan struct{})

	go func() {
		for i := 0; i < 300; i++ {
			_ = h.Fire(entry("flood"))
		}

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Fire blocked: drop-on-full publish is not holding")
	}
}
