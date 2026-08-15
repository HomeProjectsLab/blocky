package tui

import (
	"math"
	"strings"
	"testing"
)

func TestMeterBar(t *testing.T) {
	cases := []struct {
		v, m float64
		w    int
		want string
	}{
		{0, 100, 10, "          "},        // empty
		{100, 100, 10, "||||||||||"},      // full
		{50, 100, 10, "|||||     "},       // half
		{200, 100, 10, "||||||||||"},      // clamps >max
		{-5, 100, 10, "          "},       // clamps <0
		{50, 0, 10, "          "},         // max=0 → empty, no divide-by-zero
		{math.NaN(), 1, 10, "          "}, // NaN frac must not panic strings.Repeat
	}
	for _, c := range cases {
		if got := meterBar(c.v, c.m, c.w); got != c.want {
			t.Errorf("meterBar(%v,%v,%d)=%q want %q", c.v, c.m, c.w, got, c.want)
		}
	}
	if meterBar(1, 1, 0) != "" {
		t.Error("zero width must render empty")
	}
}

func TestLevelFor(t *testing.T) {
	if levelFor(0.1) != LevelGreen || levelFor(0.7) != LevelYellow || levelFor(0.95) != LevelRed {
		t.Error("threshold bands wrong")
	}
}

func TestReadSSE(t *testing.T) {
	// two query events (one real, one decoy) plus a ping comment that must be ignored.
	stream := "event: query\n" +
		`data: {"ts":"t1","client":"c","question":"real.example","qtype":"A","rtype":"RESOLVED","decoy":false}` + "\n\n" +
		": ping\n\n" +
		"event: query\n" +
		`data: {"ts":"t2","client":"c","question":"chaff.example","qtype":"A","rtype":"RESOLVED","decoy":true,"decoySource":"cohort"}` + "\n\n"

	var got []QueryItem
	if err := readSSE(strings.NewReader(stream), func(q QueryItem) { got = append(got, q) }); err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("want 2 queries, got %d", len(got))
	}
	if got[0].Question != "real.example" || got[0].Decoy {
		t.Errorf("first event wrong: %+v", got[0])
	}
	if !got[1].Decoy || got[1].DecoySource != "cohort" {
		t.Errorf("decoy provenance not parsed: %+v", got[1])
	}
}
