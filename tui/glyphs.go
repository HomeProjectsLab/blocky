package tui

// GlyphSet is the drawing alphabet for one capability tier. Box-drawing borders
// are CP437-safe on the kernel framebuffer, so they are identical in both sets;
// only the sparkline ramp, the empty-bar fill and the ticker result marks differ
// (fine 1/8-blocks and braille render as tofu on fbcon, so they degrade).
type GlyphSet struct {
	Ramp     []rune // low→high, index 0 is blank
	Bar      rune   // filled meter cell
	BarEmpty rune   // empty meter cell
	BarL     rune   // meter left edge
	BarR     rune   // meter right edge

	Blocked rune
	OK      rune
	Cached  rune
	Decoy   rune

	// box-drawing border (safe on both tiers)
	TL, TR, BL, BR, H, V, TeeL, TeeR rune
}

var unicodeGlyphs = GlyphSet{
	Ramp:     []rune(" ▁▂▃▄▅▆▇█"),
	Bar:      '█',
	BarEmpty: '▁',
	BarL:     '▕',
	BarR:     '▏',
	Blocked:  '●',
	OK:       '○',
	Cached:   '▪',
	Decoy:    '·',
	TL:       '┌', TR: '┐', BL: '└', BR: '┘', H: '─', V: '│', TeeL: '├', TeeR: '┤',
}

var fbconGlyphs = GlyphSet{
	Ramp:     []rune(" ▄█"), // half + full block only; no fine 1/8 steps
	Bar:      '█',
	BarEmpty: ' ',
	BarL:     '[',
	BarR:     ']',
	Blocked:  '*',
	OK:       'o',
	Cached:   '#',
	Decoy:    '~',
	TL:       '┌', TR: '┐', BL: '└', BR: '┘', H: '─', V: '│', TeeL: '├', TeeR: '┤',
}

func pickGlyphs(fbcon bool) GlyphSet {
	if fbcon {
		return fbconGlyphs
	}

	return unicodeGlyphs
}

// bannerFont is a 3-row × 3-col block font for the hero KPI numbers. It uses
// only ▀▄█ and space (all CP437-safe, so it renders on the Pi framebuffer too).
var bannerFont = map[rune][3]string{
	'0': {"█▀█", "█ █", "█▄█"},
	'1': {"▄█ ", " █ ", "▄█▄"},
	'2': {"█▀█", " ▄▀", "█▄▄"},
	'3': {"█▀█", " ▀█", "█▄█"},
	'4': {"█ █", "█▄█", "  █"},
	'5': {"█▀▀", "▀▀█", "▄▄█"},
	'6': {"█▀▀", "█▄█", "█▄█"},
	'7': {"▀▀█", "  █", " █ "},
	'8': {"█▀█", "█▀█", "█▄█"},
	'9': {"█▀█", "█▄█", "▄▄█"},
	'%': {"█ ▄", " ▄ ", "▀ █"},
	'.': {"   ", "   ", " ▄ "},
	' ': {"   ", "   ", "   "},
}

// bannerLines renders text as three block-font rows, space-separated per glyph.
// Unknown runes fall back to a blank cell.
func bannerLines(text string) [3]string {
	var rows [3]string

	for i, r := range text {
		g, ok := bannerFont[r]
		if !ok {
			g = bannerFont[' ']
		}

		if i > 0 {
			for j := 0; j < 3; j++ {
				rows[j] += " "
			}
		}

		for j := 0; j < 3; j++ {
			rows[j] += g[j]
		}
	}

	return rows
}
