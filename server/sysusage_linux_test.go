//go:build linux

package server

import (
	"context"
	"testing"
	"time"
)

// The linux sampler must prime, publish bounds-valid samples from /proc, and
// stop when its ctx is cancelled (the server-lifetime ctx cancelled on shutdown).
func TestSamplerPublishesAndStops(t *testing.T) {
	s := &statsAPI{} // no store -> samples the root filesystem

	ctx, cancel := context.WithCancel(context.Background())
	s.startSampler(ctx)

	var snap *sysSnapshot

	deadline := time.Now().Add(6 * time.Second) // first sample lands after ~sysSampleInterval
	for time.Now().Before(deadline) {
		if snap = s.sysUsage.Load(); snap != nil {
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	if snap == nil {
		t.Fatal("sampler never published a snapshot")
	}

	if snap.MemTotal == 0 {
		t.Error("MemTotal should be > 0")
	}

	if snap.DiskTotal == 0 {
		t.Error("DiskTotal should be > 0")
	}

	for i, p := range snap.CPUPerCore {
		if p < 0 || p > 100 {
			t.Errorf("cpu core %d out of [0,100]: %v", i, p)
		}
	}

	// cancel and confirm the goroutine stops advancing the snapshot
	cancel()
	time.Sleep(100 * time.Millisecond)
	before := s.sysUsage.Load()
	time.Sleep(sysSampleInterval + 500*time.Millisecond)

	if s.sysUsage.Load() != before {
		t.Error("sampler kept publishing after ctx cancel")
	}
}
