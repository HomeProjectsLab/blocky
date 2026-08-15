package tui

// Rect is a cell rectangle: origin (X,Y) plus width/height. All layout math is
// pure (no tcell) so the tiling invariants are unit-testable without a terminal.
type Rect struct{ X, Y, W, H int }

// Empty reports a rect with no drawable area.
func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

// Inset shrinks the rect by n cells on every side (border padding). Extents are
// clamped at zero, never negative.
func (r Rect) Inset(n int) Rect {
	out := Rect{r.X + n, r.Y + n, r.W - 2*n, r.H - 2*n}
	if out.W < 0 {
		out.W = 0
	}

	if out.H < 0 {
		out.H = 0
	}

	return out
}

// Rows carves n rows off the top, returning (top, rest). n is clamped to [0,H]
// so top+rest always tile r exactly.
func (r Rect) Rows(n int) (top, rest Rect) {
	n = clampInt(n, 0, r.H)

	return Rect{r.X, r.Y, r.W, n}, Rect{r.X, r.Y + n, r.W, r.H - n}
}

// RowsBottom carves n rows off the bottom, returning (rest, bottom).
func (r Rect) RowsBottom(n int) (rest, bottom Rect) {
	n = clampInt(n, 0, r.H)

	return Rect{r.X, r.Y, r.W, r.H - n}, Rect{r.X, r.Y + r.H - n, r.W, n}
}

// Cols carves n columns off the left, returning (left, rest).
func (r Rect) Cols(n int) (left, rest Rect) {
	n = clampInt(n, 0, r.W)

	return Rect{r.X, r.Y, n, r.H}, Rect{r.X + n, r.Y, r.W - n, r.H}
}

// SplitV divides r into horizontal bands stacked top-to-bottom, one per weight.
// Heights are proportional to weights; the rounding remainder is handed to the
// last positive-weight band, so the bands tile r exactly (sum of heights == r.H,
// no gaps, no overlap). Zero-weight entries get an empty band.
func (r Rect) SplitV(weights ...int) []Rect {
	return split(r.H, weights, func(off, ext int) Rect {
		return Rect{r.X, r.Y + off, r.W, ext}
	})
}

// SplitH divides r into vertical columns left-to-right by weight, same tiling
// guarantee as SplitV.
func (r Rect) SplitH(weights ...int) []Rect {
	return split(r.W, weights, func(off, ext int) Rect {
		return Rect{r.X + off, r.Y, ext, r.H}
	})
}

func split(total int, weights []int, mk func(off, ext int) Rect) []Rect {
	out := make([]Rect, len(weights))

	sum, lastPos := 0, -1

	for i, w := range weights {
		if w > 0 {
			sum += w
			lastPos = i
		}
	}

	if sum <= 0 || total <= 0 {
		for i := range out {
			out[i] = mk(0, 0)
		}

		return out
	}

	ext := make([]int, len(weights))
	used := 0

	for i, w := range weights {
		if w > 0 {
			ext[i] = w * total / sum
			used += ext[i]
		}
	}

	ext[lastPos] += total - used // remainder keeps the tiling exact

	off := 0
	for i := range weights {
		out[i] = mk(off, ext[i])
		off += ext[i]
	}

	return out
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}

	if v > hi {
		return hi
	}

	return v
}

// tier is the reactive layout band chosen from the terminal size.
type tier int

const (
	tierTiny tier = iota
	tierMedium
	tierLarge
)

// pickTier maps (cols,rows) to a layout band: TINY below 80×20, LARGE at HDMI
// width (≥140 cols), MEDIUM in between.
func pickTier(w, h int) tier {
	switch {
	case w < 80 || h < 20:
		return tierTiny
	case w < 140:
		return tierMedium
	default:
		return tierLarge
	}
}
