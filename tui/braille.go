package tui

import "github.com/gdamore/tcell/v2"

// brailleBit maps a 2×4 dot coordinate to its Unicode braille bit. Standard dot
// numbering: dot1=0x01 dot4=0x08 / dot2=0x02 dot5=0x10 / dot3=0x04 dot6=0x20 /
// dot7=0x40 dot8=0x80.
var brailleBit = [2][4]uint8{{0x01, 0x02, 0x04, 0x40}, {0x08, 0x10, 0x20, 0x80}}

// Braille is a dot canvas of cols×rows cells, each cell a 2×4 dot block. The dot
// grid is 2*cols × 4*rows with origin top-left.
type Braille struct {
	cols, rows int
	cell       []uint8 // cell[cx+cy*cols]
}

func newBraille(cols, rows int) *Braille { return &Braille{cols, rows, make([]uint8, cols*rows)} }

// set lights dot (px,py), px 0..2cols-1, py 0..4rows-1. Out-of-range is a no-op.
func (b *Braille) set(px, py int) {
	if px < 0 || py < 0 || px >= 2*b.cols || py >= 4*b.rows {
		return
	}

	b.cell[(px/2)+(py/4)*b.cols] |= brailleBit[px%2][py%4]
}

// blit paints the lit cells into r; empty cells are skipped so the panel backdrop
// stays transparent.
func (b *Braille) blit(s tcell.Screen, r Rect, style tcell.Style) {
	for cy := 0; cy < b.rows; cy++ {
		for cx := 0; cx < b.cols; cx++ {
			if v := b.cell[cx+cy*b.cols]; v != 0 {
				setCell(s, r.X+cx, r.Y+cy, rune(0x2800+int(v)), style)
			}
		}
	}
}

// drawGraph fills r with an area graph of values (oldest→newest, newest at
// right), scaled to maxv. A caller-supplied maxv is a sticky scale that stops the
// graph rescaling (flapping) every frame; maxv<=0 falls back to the window max.
// Braille caps → 2×4 dot canvas filled from baseline up in the style's accent.
// Else (fbcon) → height-filling block columns using only █ ▄ space (CP437-safe).
func drawGraph(s tcell.Screen, r Rect, caps Caps, style tcell.Style, values []float64, maxv float64) {
	if r.Empty() {
		return
	}

	if maxv <= 0 {
		for _, v := range values {
			if v > maxv {
				maxv = v
			}
		}
	}

	if maxv <= 0 {
		return
	}

	if caps.Braille {
		b := newBraille(r.W, r.H)
		dots := 2 * r.W
		n := len(values)

		// Resample the series across the FULL dot width (oldest at left, newest at
		// right) so a short/cold-start history spans the panel instead of hugging
		// the right edge; a long history is nearest-sampled down to the width.
		for px := 0; px < dots; px++ {
			v := values[n-1]
			if n > 1 && dots > 1 {
				idx := int(float64(px)/float64(dots-1)*float64(n-1) + 0.5)
				v = values[idx]
			}

			h := int(v/maxv*float64(4*r.H) + 0.5)
			for py := 4*r.H - 1; py >= 4*r.H-h && py >= 0; py-- {
				b.set(px, py)
			}
		}

		b.blit(s, r, style)

		return
	}

	// fbcon: one block column per cell, ▄ = half-cell vertical resolution. Same
	// full-width resample as the braille path so the graph spans the panel.
	n := len(values)
	for cx := 0; cx < r.W; cx++ {
		v := values[n-1]
		if n > 1 && r.W > 1 {
			idx := int(float64(cx)/float64(r.W-1)*float64(n-1) + 0.5)
			v = values[idx]
		}

		half := int(v/maxv*float64(2*r.H) + 0.5) // 0..2H half-cells
		x := r.X + cx

		for row := 0; row < r.H; row++ {
			y := r.Y + r.H - 1 - row

			switch fromBottom := half - row*2; {
			case fromBottom >= 2:
				setCell(s, x, y, '█', style)
			case fromBottom == 1:
				setCell(s, x, y, '▄', style)
			}
		}
	}
}
