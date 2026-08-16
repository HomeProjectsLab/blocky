//go:generate go tool go-enum -f=$GOFILE --marshal --names --values
package config

import (
	"fmt"
	"strings"
)

const maxTextSourceDisplayLen = 12

// var BytesSourceNone = BytesSource{}

// BytesSourceType supported BytesSource types. ENUM(
// text=1 // Inline YAML block.
// http   // HTTP(S).
// file   // Local file.
// )
type BytesSourceType uint16

type BytesSource struct {
	Type BytesSourceType
	From string
}

func (s BytesSource) String() string {
	switch s.Type {
	case BytesSourceTypeText:
		break

	case BytesSourceTypeHttp:
		return s.From

	case BytesSourceTypeFile:
		return "file://" + s.From

	default:
		return fmt.Sprintf("unknown source (%s: %s)", s.Type, s.From)
	}

	text := s.From
	truncated := false

	if idx := strings.IndexRune(text, '\n'); idx != -1 {
		truncated = idx < len(text)-1 // content after the first line's newline
		text = text[:idx]             // first line only
	}

	if len(text) > maxTextSourceDisplayLen { // truncate
		text = text[:maxTextSourceDisplayLen]
		truncated = true
	}

	if truncated {
		return text + "..."
	}

	return text
}

// UnmarshalText implements `encoding.TextUnmarshaler`.
func (s *BytesSource) UnmarshalText(data []byte) error {
	source := string(data)

	switch {
	// Inline definition in YAML (with literal style Block Scalar)
	case strings.ContainsAny(source, "\n"):
		*s = BytesSource{Type: BytesSourceTypeText, From: source}

	// HTTP(S) — require the scheme separator so a local path like
	// "httpdocs/ads.txt" isn't treated as a URL (which would fail-open the list)
	case strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		*s = BytesSource{Type: BytesSourceTypeHttp, From: source}

	// Probably path to a local file
	default:
		*s = BytesSource{Type: BytesSourceTypeFile, From: strings.TrimPrefix(source, "file://")}
	}

	return nil
}

func newBytesSource(source string) BytesSource {
	var res BytesSource

	// UnmarshalText never returns an error
	_ = res.UnmarshalText([]byte(source))

	return res
}

func NewBytesSources(sources ...string) []BytesSource {
	res := make([]BytesSource, 0, len(sources))

	for _, source := range sources {
		res = append(res, newBytesSource(source))
	}

	return res
}

func TextBytesSource(lines ...string) BytesSource {
	return BytesSource{Type: BytesSourceTypeText, From: inlineList(lines...)}
}

func inlineList(lines ...string) string {
	res := strings.Join(lines, "\n")

	// ensure at least one line ending so it's parsed as an inline block
	res += "\n"

	return res
}
