package configstore

import (
	"context"
	"testing"
	"time"
)

// TestSessionSecretFreezesAuthUnderDBStall reproduces the operator's live boot
// hang: with the decoy engine enabled, the querylog.db IO storm saturates the
// Pi so that config.db's single connection cannot make progress. SessionSecret()
// holds Store.s.mu across an UNCACHED config.db read on that 1-connection pool
// (auth.go:126 -> store.go authRow -> s.conn().First), so the moment that read
// stalls, s.mu is pinned and EVERY other authenticated request — each of which
// also calls SessionSecret() (server/auth.go) — blocks forever on s.mu.
//
// It is timeout-proof on the live box because sync.Mutex.Lock has no context and
// authRow's First() carries context.Background() (no deadline); with the pool
// pinned to MaxOpenConns=1 the second reader waits UNBOUNDED at the database/sql
// pool layer (busy_timeout does not bound pool-acquire).
//
// We reproduce the IO stall deterministically by checking out config.db's only
// connection, then showing that a normal second authenticated request freezes.
// The recommended fix (cache the secret in memory so the auth hot path never
// holds s.mu across a DB read) would make this test pass: the cached read needs
// no connection, so goroutine B returns immediately.
func TestSessionSecretFreezesAuthUnderDBStall(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	// Seed the secret row once (uncontended) so both callers below hit the
	// steady-state "row exists" read path, not first-time generation.
	if _, err := s.SessionSecret(); err != nil {
		t.Fatalf("seed session secret: %v", err)
	}

	// Simulate the decoy IO storm: config.db's lone connection cannot make
	// progress. Check the single pool connection out and never give it back for
	// the duration of the freeze window. Any other s.conn() read now blocks at
	// the database/sql pool layer with no deadline.
	sqlDB, err := s.conn().DB()
	if err != nil {
		t.Fatalf("get sql.DB: %v", err)
	}

	heldConn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("reserve connection: %v", err)
	}
	defer heldConn.Close() // release so the parked goroutines drain at teardown

	// Goroutine A = one authenticated request. It grabs s.mu, then blocks inside
	// authRow's First() waiting for the (checked-out) connection — holding s.mu.
	aGrabbedMu := make(chan struct{})
	go func() {
		// SessionSecret takes s.mu first, then does the stalled DB read.
		close(aGrabbedMu)
		_, _ = s.SessionSecret()
	}()
	<-aGrabbedMu
	// Give A a moment to acquire s.mu and enter the blocked DB read.
	time.Sleep(150 * time.Millisecond)

	// Goroutine B = a SECOND, unrelated authenticated request. In a healthy
	// design its secret read is independent; here it blocks on s.mu, which A
	// holds across the stalled read. This is the total auth freeze.
	bDone := make(chan struct{})
	go func() {
		_, _ = s.SessionSecret()
		close(bDone)
	}()

	select {
	case <-bDone:
		// Auth path stayed responsive — bug is fixed (or absent).
	case <-time.After(2 * time.Second):
		t.Fatalf("DEADLOCK REPRODUCED: a second authenticated request (SessionSecret) " +
			"froze on Store.s.mu while it was held across a stalled config.db read. " +
			"On the live Pi this is the total auth freeze; DNS dies from the same " +
			"decoy-induced saturation. Fix: cache the session secret so the auth path " +
			"never holds s.mu across a DB read.")
	}
}
