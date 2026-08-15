package tui

// sparkline renders values (oldest→newest) as a single row of width cells,
// auto-scaled to the current max. It draws the newest `width` samples right-
// aligned, left-padding with blanks. The ramp comes from GlyphSet, so on fbcon
// it degrades to a 3-level block ramp (never braille/fine-block on the Pi).
func sparkline(values []float64, width int, g GlyphSet) string {
	if width <= 0 {
		return ""
	}

	ramp := g.Ramp
	if len(ramp) < 2 {
		ramp = []rune(" █")
	}

	out := make([]rune, width)
	for i := range out {
		out[i] = ramp[0]
	}

	if len(values) == 0 {
		return string(out)
	}

	max := 0.0
	for _, v := range values {
		if v > max {
			max = v
		}
	}

	start := 0
	if len(values) > width {
		start = len(values) - width
	}

	vis := values[start:]
	offset := width - len(vis)

	for i, v := range vis {
		lvl := 0
		if max > 0 {
			lvl = int(v/max*float64(len(ramp)-1) + 0.5)
		}

		if lvl < 0 {
			lvl = 0
		}

		if lvl > len(ramp)-1 {
			lvl = len(ramp) - 1
		}

		out[offset+i] = ramp[lvl]
	}

	return string(out)
}
