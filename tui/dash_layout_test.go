package tui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

// ---- layout math ----

// tileV asserts a vertical split tiles the parent exactly: no gap, no overlap,
// heights sum to the parent and each band keeps the parent's X/W.
func tileV(t *testing.T, parent Rect, parts []Rect) {
	t.Helper()

	sum, y := 0, parent.Y
	for i, p := range parts {
		if p.X != parent.X || p.W != parent.W {
			t.Errorf("band %d X/W drifted: %+v vs parent %+v", i, p, parent)
		}
		if p.Y != y {
			t.Errorf("band %d gap/overlap: Y=%d want %d", i, p.Y, y)
		}
		y += p.H
		sum += p.H
	}
	if sum != parent.H {
		t.Errorf("heights sum %d != parent H %d", sum, parent.H)
	}
}

func tileH(t *testing.T, parent Rect, parts []Rect) {
	t.Helper()

	sum, x := 0, parent.X
	for i, p := range parts {
		if p.Y != parent.Y || p.H != parent.H {
			t.Errorf("col %d Y/H drifted: %+v vs parent %+v", i, p, parent)
		}
		if p.X != x {
			t.Errorf("col %d gap/overlap: X=%d want %d", i, p.X, x)
		}
		x += p.W
		sum += p.W
	}
	if sum != parent.W {
		t.Errorf("widths sum %d != parent W %d", sum, parent.W)
	}
}

func TestSplitTilesExactly(t *testing.T) {
	sizes := []Rect{{0, 0, 160, 48}, {5, 5, 79, 20}, {0, 0, 141, 21}, {0, 0, 7, 3}}
	for _, r := range sizes {
		tileV(t, r, r.SplitV(1, 1, 1))
		tileV(t, r, r.SplitV(2, 3))
		tileH(t, r, r.SplitH(6, 3, 5, 6))
		tileH(t, r, r.SplitH(1, 2))
	}
}

func TestSplitWeightDistribution(t *testing.T) {
	r := Rect{0, 0, 100, 10}
	parts := r.SplitH(3, 2) // 60/40
	if parts[0].W != 60 || parts[1].W != 40 {
		t.Errorf("weight split = %d/%d, want 60/40", parts[0].W, parts[1].W)
	}
	// remainder goes to the last positive band, tiling stays exact.
	tileH(t, r, r.SplitH(1, 1, 1))
}

func TestSplitZeroWeightGetsEmptyBand(t *testing.T) {
	r := Rect{0, 0, 30, 4}
	parts := r.SplitH(0, 1)
	if parts[0].W != 0 || parts[1].W != 30 {
		t.Errorf("zero-weight band = %d/%d, want 0/30", parts[0].W, parts[1].W)
	}
	tileH(t, r, parts)
}

func TestSplitDegenerateSafe(t *testing.T) {
	for _, r := range []Rect{{0, 0, 0, 0}, {0, 0, 1, 1}, {0, 0, 10, 0}} {
		for _, p := range r.SplitV(1, 2, 3) {
			if p.W < 0 || p.H < 0 {
				t.Errorf("degenerate %+v produced negative rect %+v", r, p)
			}
		}
	}
}

func TestRowsColsAndInset(t *testing.T) {
	r := Rect{0, 0, 20, 10}
	top, rest := r.Rows(3)
	if top.H != 3 || rest.H != 7 || rest.Y != 3 {
		t.Errorf("Rows(3) = %+v / %+v", top, rest)
	}
	// clamp beyond bounds
	if tp, rt := r.Rows(99); tp.H != 10 || rt.H != 0 {
		t.Errorf("Rows overflow not clamped: %+v / %+v", tp, rt)
	}
	body, bottom := r.RowsBottom(2)
	if bottom.Y != 8 || bottom.H != 2 || body.H != 8 {
		t.Errorf("RowsBottom(2) = %+v / %+v", body, bottom)
	}
	l, rr := r.Cols(6)
	if l.W != 6 || rr.X != 6 || rr.W != 14 {
		t.Errorf("Cols(6) = %+v / %+v", l, rr)
	}
	if in := r.Inset(2); in.X != 2 || in.Y != 2 || in.W != 16 || in.H != 6 {
		t.Errorf("Inset(2) = %+v", in)
	}
	if in := (Rect{0, 0, 2, 2}).Inset(3); in.W != 0 || in.H != 0 {
		t.Errorf("Inset past zero must clamp: %+v", in)
	}
}

// ---- breakpoint selection ----

func TestPickTierBoundaries(t *testing.T) {
	cases := []struct {
		w, h int
		want tier
	}{
		{79, 40, tierTiny}, {80, 19, tierTiny}, {80, 20, tierMedium},
		{139, 20, tierMedium}, {140, 20, tierLarge}, {200, 60, tierLarge},
	}
	for _, c := range cases {
		if got := pickTier(c.w, c.h); got != c.want {
			t.Errorf("pickTier(%d,%d)=%v want %v", c.w, c.h, got, c.want)
		}
	}
}

// ---- capability detection ----

func TestDetectFbconVsUnicode(t *testing.T) {
	if c := Detect(8, "linux", false); c.Unicode || c.Braille {
		t.Errorf("TERM=linux must be fbcon: %+v", c)
	}
	if c := Detect(256, "vt220", false); c.Unicode {
		t.Error("TERM=vt* must be fbcon")
	}
	if c := Detect(1<<24, "xterm-256color", false); !c.Unicode || !c.Braille {
		t.Error("xterm must be unicode/braille")
	}
	// env override forces fbcon regardless of TERM
	if c := Detect(1<<24, "xterm-256color", true); c.Unicode {
		t.Error("BLOCKY_DASH_ASCII override must force fbcon")
	}
}

func TestAccentColorBands(t *testing.T) {
	// 8-colour tier collapses to named threshold colours.
	c8 := Detect(8, "linux", false)
	if c8.Accent(0.1) != LevelGreen.Color() || c8.Accent(0.95) != LevelRed.Color() {
		t.Error("8-colour Accent must use named threshold colours")
	}
	// truecolor tier produces distinct RGB for low vs high.
	ct := Detect(1<<24, "xterm", false)
	if ct.Accent(0.1) == ct.Accent(0.95) {
		t.Error("truecolor Accent should differ across the gradient")
	}
}

// ---- glyphs + sparkline ----

func TestPickGlyphsAndWidths(t *testing.T) {
	fb := pickGlyphs(true)
	if fb.Decoy != '~' || len(fb.Ramp) != 3 {
		t.Errorf("fbcon glyphs wrong: %+v", fb)
	}
	uni := pickGlyphs(false)
	if uni.Decoy != '·' || len(uni.Ramp) < 8 {
		t.Errorf("unicode glyphs wrong: %+v", uni)
	}
	// every drawable glyph must be exactly one cell wide (column math safety).
	for _, g := range []GlyphSet{fb, uni} {
		var runes []rune
		runes = append(runes, g.Ramp...)
		runes = append(runes, g.Bar, g.BarEmpty, g.BarL, g.BarR, g.Blocked, g.OK, g.Cached, g.Decoy,
			g.TL, g.TR, g.BL, g.BR, g.H, g.V, g.TeeL, g.TeeR)
		for _, r := range runes {
			if r == ' ' {
				continue
			}
			if w := runewidth.RuneWidth(r); w != 1 {
				t.Errorf("glyph %q width %d, want 1", r, w)
			}
		}
	}
}

func TestBannerFontWidthsAndRunes(t *testing.T) {
	for r, g := range bannerFont {
		for _, row := range g {
			if n := len([]rune(row)); n != 3 {
				t.Errorf("banner %q row %q is %d cells, want 3", r, row, n)
			}
			if w := runewidth.StringWidth(row); w != 3 {
				t.Errorf("banner %q row %q display width %d, want 3", r, row, w)
			}
		}
	}
	lines := bannerLines("5%")
	if len(lines[0]) == 0 || !strings.Contains(strings.Join(lines[:], ""), "█") {
		t.Errorf("bannerLines(5%%) empty: %+v", lines)
	}
}

func TestSparklinePerTier(t *testing.T) {
	vals := []float64{0, 1, 2, 3, 4, 5, 6, 7}

	uni := sparkline(vals, 8, unicodeGlyphs)
	if []rune(uni)[0] != ' ' || []rune(uni)[7] != '█' {
		t.Errorf("unicode sparkline = %q, want ramp low..full", uni)
	}

	fb := sparkline(vals, 8, fbconGlyphs)
	for _, r := range fb {
		if r != ' ' && r != '▄' && r != '█' {
			t.Errorf("fbcon sparkline used non-3-level glyph %q in %q", r, fb)
		}
	}

	if sparkline(nil, 5, unicodeGlyphs) != "     " {
		t.Error("empty series must render blanks")
	}
	if sparkline(vals, 0, unicodeGlyphs) != "" {
		t.Error("zero width must render empty")
	}
}

// ---- formatting ----

func TestFmtHelpers(t *testing.T) {
	if got := fmtBytes(22 * 1024 * 1024); got != "22M" {
		t.Errorf("fmtBytes 22MiB = %q", got)
	}
	if got := fmtBytes(2202009600); got != "2.1G" {
		t.Errorf("fmtBytes ~2.1G = %q", got)
	}
	if got := fmtBytes(512); got != "512B" {
		t.Errorf("fmtBytes 512 = %q", got)
	}
	if got := fmtUptime(3*86400 + 4*3600 + 12*60); got != "3d 4h 12m" {
		t.Errorf("fmtUptime = %q", got)
	}
	if got := fmtUptime(90); got != "1m" {
		t.Errorf("fmtUptime 90s = %q", got)
	}
	if got := fmtCount(1_240_000); got != "1.2M" {
		t.Errorf("fmtCount M = %q", got)
	}
	if got := fmtCount(384_000); got != "384K" {
		t.Errorf("fmtCount K = %q", got)
	}
	if got := fmtCount(42); got != "42" {
		t.Errorf("fmtCount small = %q", got)
	}
}
