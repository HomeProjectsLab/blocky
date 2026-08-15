package decoy

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/querylog"
)

// TestRunDispatchOverlaps proves the fix for the under-generation root cause: the
// Run timer loop must NOT serialize behind a slow emit()/resolve. With a blocking
// resolve, a serial loop would only ever have 1 emission in flight; the bounded
// worker pool lets several overlap (capped at maxConcurrentEmits).
func TestRunDispatchOverlaps(t *testing.T) {
	cfg, err := config.WithDefaults[config.DecoyConfig]()
	if err != nil {
		t.Fatal(err)
	}

	cfg.Enable = true
	cfg.MissChaffPct = 100 // every emit = one synchronous resolveOne(miss), no async cohort
	cfg.PersonaCover = false
	cfg.ReactiveVolume = false
	cfg.DiurnalShaping = false
	cfg.AdaptiveBackoff = false
	cfg.ShadowTTL = false
	cfg.DeviceClass.Enable = false
	cfg.QueriesPerMinute = 6000 // ~100 ticks/sec so the pool fills fast

	src, err := querylog.NewDecoySource(filepath.Join(t.TempDir(), "decoy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	if _, err = src.SeedIfEmpty(strings.NewReader("chaffparent.example\n")); err != nil {
		t.Fatal(err)
	}

	var (
		mu      sync.Mutex
		inFlt   int
		maxInFl int
	)

	resolve := func(ctx context.Context, _ *model.Request) (*model.Response, error) {
		mu.Lock()
		inFlt++
		if inFlt > maxInFl {
			maxInFl = inFlt
		}
		mu.Unlock()

		select {
		case <-ctx.Done():
		case <-time.After(20 * time.Millisecond): // slow upstream
		}

		mu.Lock()
		inFlt--
		mu.Unlock()

		return &model.Response{}, nil
	}

	eng := NewEngine(cfg, src, resolve)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()
	<-done

	mu.Lock()
	peak := maxInFl
	mu.Unlock()

	if peak < 2 {
		t.Fatalf("emit loop serialized (peak concurrency %d) — Run is not dispatching to the worker pool", peak)
	}

	if peak > maxConcurrentEmits {
		t.Fatalf("peak concurrency %d exceeds cap %d — pool bound is broken", peak, maxConcurrentEmits)
	}
}
