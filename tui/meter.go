package tui

import (
	"strings"

	"github.com/gdamore/tcell/v2"
)

// Level is a green/yellow/red threshold band for a meter.
type Level int

const (
	LevelGreen Level = iota
	LevelYellow
	LevelRed
)

// levelFor maps a fill fraction (0..1) to a threshold band: <0.6 green,
// <0.85 yellow, else red.
func levelFor(frac float64) Level {
	switch {
	case frac >= 0.85:
		return LevelRed
	case frac >= 0.6:
		return LevelYellow
	default:
		return LevelGreen
	}
}

func (l Level) Color() tcell.Color {
	switch l { //nolint:exhaustive // default covers LevelGreen
	case LevelRed:
		return tcell.ColorRed
	case LevelYellow:
		return tcell.ColorYellow
	default:
		return tcell.ColorGreen
	}
}

// meterBar renders an htop-style ASCII gauge body, e.g. "|||||     ". Kept for
// the legacy path and tests; new panels use meterBarWith with caps glyphs.
func meterBar(value, max float64, width int) string {
	return meterBarWith(value, max, width, '|', ' ')
}

// meterBarWith renders a gauge body of the given inner width using caller-chosen
// fill/empty runes (e.g. '█'/'▁' on unicode, '█'/' ' on fbcon). value/max is
// clamped to 0..1.
func meterBarWith(value, max float64, width int, fill, empty rune) string {
	if width <= 0 {
		return ""
	}

	frac := 0.0
	if max > 0 {
		frac = value / max
	}

	if frac < 0 {
		frac = 0
	}

	if frac > 1 {
		frac = 1
	}

	if frac != frac { // NaN: comparisons above are false, guard before the int cast.
		frac = 0
	}

	filled := int(frac*float64(width) + 0.5)

	return strings.Repeat(string(fill), filled) + strings.Repeat(string(empty), width-filled)
}
