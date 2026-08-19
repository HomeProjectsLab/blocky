package tui

import "testing"

// A single (or short) run of failed polls must keep the last good frame on screen
// (connected stays true) — the flapping fix. Only disconnectGrace consecutive
// failures drop to the splash, and one success re-arms the grace.
func TestApplyPollResultHysteresis(t *testing.T) {
	d := New(NewClient("http://x"), 0)

	d.applyPollResult(true)
	if !d.connected {
		t.Fatal("first success must connect")
	}

	// up to grace-1 consecutive failures keep the frame
	for i := 1; i < disconnectGrace; i++ {
		d.applyPollResult(false)
		if !d.connected {
			t.Fatalf("failure %d/%d blanked the UI; must tolerate < grace", i, disconnectGrace)
		}
	}

	// the grace-th consecutive failure finally drops to splash
	d.applyPollResult(false)
	if d.connected {
		t.Fatalf("%d consecutive failures must disconnect", disconnectGrace)
	}

	// one success re-arms: a later single blip must not blank again
	d.applyPollResult(true)
	d.applyPollResult(false)
	if !d.connected {
		t.Fatal("success must reset the streak; a single later failure must not disconnect")
	}
}
