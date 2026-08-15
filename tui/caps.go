package tui

import (
	"os"
	"strings"

	"github.com/gdamore/tcell/v2"
)

// Caps is the terminal's rendering capability, detected once at startup and
// threaded through every panel. No per-frame probing.
type Caps struct {
	Color   int // screen.Colors(): 8 / 256 / 1<<24
	Unicode bool
	Braille bool
	Glyphs  GlyphSet
}

// Detect chooses a capability tier from the colour count and TERM. The kernel
// framebuffer console (TERM=linux/vt*/console) has no braille and no fine blocks
// in its font, so it drops to the block/ASCII glyph set. asciiEnv forces that
// same fbcon path regardless of TERM (BLOCKY_DASH_ASCII=1) — the calibration
// knob for a display the process cannot introspect.
func Detect(colors int, term string, asciiEnv bool) Caps {
	fbcon := term == "linux" || strings.HasPrefix(term, "vt") || term == "console"
	if asciiEnv {
		fbcon = true
	}

	return Caps{
		Color:   colors,
		Unicode: !fbcon,
		Braille: !fbcon,
		Glyphs:  pickGlyphs(fbcon),
	}
}

// DetectScreen reads the live terminal capabilities off an initialised screen.
func DetectScreen(s tcell.Screen) Caps {
	return Detect(s.Colors(), os.Getenv("TERM"), os.Getenv("BLOCKY_DASH_ASCII") == "1")
}

// Accent picks a level colour for a 0..1 fraction. On a truecolor terminal it is
// a smooth green→amber→red gradient; below that it collapses to the three named
// threshold colours (which are all that fbcon's 8-colour palette can show).
func (c Caps) Accent(frac float64) tcell.Color {
	if c.Color >= 1<<24 {
		return gradGYR(frac)
	}

	return levelFor(frac).Color()
}

func gradGYR(f float64) tcell.Color {
	if f < 0 {
		f = 0
	}

	if f > 1 {
		f = 1
	}

	var r, g, b int32

	if f < 0.6 { // green → amber
		t := f / 0.6
		r = 40 + int32(t*(220-40))
		g = 200
		b = 80 - int32(t*80)
	} else { // amber → red
		t := (f - 0.6) / 0.4
		r = 220
		g = 200 - int32(t*(200-40))
		b = 40
	}

	return tcell.NewRGBColor(r, g, b)
}
