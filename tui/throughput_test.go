package tui

import "testing"

// The graph scale must jump UP to a new peak at once (so a burst is never clipped)
// but ease DOWN slowly (so the graph doesn't rescale/flap as peaks slide out).
func TestNextGraphMax(t *testing.T) {
	if got := nextGraphMax(0, 42); got != 42 {
		t.Fatalf("cold start must adopt the window max: got %v want 42", got)
	}

	if got := nextGraphMax(20, 50); got != 50 {
		t.Fatalf("a higher peak must be adopted immediately: got %v want 50", got)
	}

	// window max dropped 42 -> 10: scale must ease down, staying well above 10 for
	// this tick (no snap), and converge toward 10 over many ticks.
	one := nextGraphMax(42, 10)
	if one <= 10 || one >= 42 {
		t.Fatalf("ease-down must move partway, not snap: got %v", one)
	}

	cur := 42.0
	for i := 0; i < 500; i++ {
		cur = nextGraphMax(cur, 10)
	}

	if cur < 9.9 || cur > 10.1 {
		t.Fatalf("scale must converge to the window max: got %v want ~10", cur)
	}
}

func TestFmtQPS(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{
		{-1, "0.0"}, {0, "0.0"}, {0.4, "0.4"}, {9.9, "9.9"}, {10, "10"}, {42.6, "43"},
	} {
		if got := fmtQPS(c.in); got != c.want {
			t.Errorf("fmtQPS(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
