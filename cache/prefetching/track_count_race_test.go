package prefetching

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Regression: concurrent first-queries for the same key must share ONE counter;
// the old Get→create→Put path let a second racer overwrite the first counter,
// losing increments and delaying prefetch near the threshold.
func TestTrackCacheKeyQueryCountConcurrent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := NewPrefetchingCache[string](ctx, PrefetchingOptions[string]{
		PrefetchThreshold: 100,
		PrefetchExpires:   time.Minute,
	})

	const goroutines = 64

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)

		go func() {
			defer wg.Done()
			c.trackCacheKeyQueryCount("key")
		}()
	}

	wg.Wait()

	cnt, _ := c.prefetchingNameCache.Get("key")
	if cnt == nil || cnt.Load() != goroutines {
		got := uint32(0)
		if cnt != nil {
			got = cnt.Load()
		}

		t.Fatalf("expected %d tracked queries, got %d (lost increments)", goroutines, got)
	}
}
