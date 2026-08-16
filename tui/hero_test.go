package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
)

// heroFont must be a clean 5-row × 4-col block font: every row exactly 4 runes
// and 4 display columns (column math + fbcon safety).
func TestHeroFontWidthsAndRunes(t *testing.T) {
	for r, g := range heroFont {
		for _, row := range g {
			if n := len([]rune(row)); n != 4 {
				t.Errorf("hero %q row %q is %d runes, want 4", r, row, n)
			}
			if w := runewidth.StringWidth(row); w != 4 {
				t.Errorf("hero %q row %q width %d, want 4", r, row, w)
			}
		}
	}

	// three glyphs → 4+1+4+1+4 = 14 columns.
	if w := runewidth.StringWidth(heroLines("31%")[0]); w != 14 {
		t.Errorf("heroLines(31%%)[0] width %d, want 14", w)
	}
}

// braille dot (1,0) is dot4 = bit 0x08.
func TestBrailleSetBit(t *testing.T) {
	b := newBraille(1, 1)
	b.set(1, 0)
	if b.cell[0] != 0x08 {
		t.Errorf("set(1,0) → %#x, want 0x08", b.cell[0])
	}
}

// heroDraw h+v centers the number: left and right margins balance within a cell.
func TestHeroDrawCentered(t *testing.T) {
	sc := mtScreen(t, 24, 7)
	heroDraw(sc, Rect{0, 0, 24, 7}, tcell.StyleDefault, "42")
	sc.Show()

	cells, w, _ := sc.GetContents()
	minX, maxX := 1<<30, -1
	for i, c := range cells {
		if len(c.Runes) > 0 && c.Runes[0] != ' ' && c.Runes[0] != 0 {
			x := i % w
			if x < minX {
				minX = x
			}
			if x > maxX {
				maxX = x
			}
		}
	}
	if maxX < 0 {
		t.Fatal("heroDraw drew nothing")
	}
	left, right := minX, w-1-maxX
	if left-right > 1 || right-left > 1 {
		t.Errorf("hero not centered: left=%d right=%d (span %d..%d)", left, right, minX, maxX)
	}
}

// drawGraph emits braille only when Caps.Braille; fbcon uses block columns.
func TestDrawGraphBrailleGating(t *testing.T) {
	vals := []float64{1, 4, 2, 8, 5, 9, 3}

	uni := mtScreen(t, 20, 4)
	drawGraph(uni, Rect{0, 0, 20, 4}, Detect(1<<24, "xterm", false), tcell.StyleDefault, vals)
	uni.Show()
	if !strings.ContainsFunc(mtScreenText(uni), func(r rune) bool { return r >= 0x2800 && r <= 0x28FF }) {
		t.Error("braille caps: drawGraph should emit braille dots")
	}

	fb := mtScreen(t, 20, 4)
	drawGraph(fb, Rect{0, 0, 20, 4}, Detect(8, "linux", false), tcell.StyleDefault, vals)
	fb.Show()
	for _, r := range mtScreenText(fb) {
		if r >= 0x2800 && r <= 0x28FF {
			t.Errorf("fbcon: drawGraph emitted braille %q", r)
		}
		if r != ' ' && r != '█' && r != '▄' && r != '\n' {
			t.Errorf("fbcon: drawGraph emitted disallowed rune %q", r)
		}
	}
}

// The LIVE QUERIES ticker must be right-sized on LARGE, not eating half the
// screen: its box spans well under half of a 48-row terminal.
func TestTickerHeightBoundedLarge(t *testing.T) {
	sc := mtScreen(t, 160, 48)
	d := cannedDash(Detect(1<<24, "xterm-256color", false))
	d.draw(sc)
	sc.Show()

	rows := strings.Split(mtScreenText(sc), "\n")
	top := -1
	for i, r := range rows {
		if strings.Contains(r, "LIVE QUERIES") {
			top = i
			break
		}
	}
	if top < 0 {
		t.Fatal("no LIVE QUERIES panel")
	}
	// find the closing border row after the title
	bottom := -1
	for i := top + 1; i < len(rows); i++ {
		if strings.HasPrefix(strings.TrimSpace(rows[i]), "╰") {
			bottom = i
			break
		}
	}
	if bottom < 0 {
		t.Fatal("ticker box not closed")
	}
	if h := bottom - top + 1; h > 13 {
		t.Errorf("ticker box %d rows tall, want ≤13 (was ~24)", h)
	}
}
