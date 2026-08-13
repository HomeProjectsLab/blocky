package decoy

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"fmt"
	"io"
)

// trancoGz is the embedded decoy domain list. The shipped file is a PLACEHOLDER
// (a few hundred popular domains); replace it with a real Tranco 1M snapshot via
// `go run ./tools/gen-decoy-list`. The engine treats it as an opaque list of any
// size, so swapping in the real 1M list needs no code change.
//
//go:embed tranco-1m.txt.gz
var trancoGz []byte

// OpenList returns a reader over the decompressed embedded Tranco list (one
// domain per line, rank order), for callers outside this package (the prewarm
// worker mines its mid-popularity band). Caller must Close it.
func OpenList() (io.ReadCloser, error) { return openList() }

// openList returns a reader over the decompressed embedded list (one domain per
// line). Caller must Close it.
func openList() (io.ReadCloser, error) {
	gr, err := gzip.NewReader(bytes.NewReader(trancoGz))
	if err != nil {
		return nil, fmt.Errorf("can't open embedded decoy list: %w", err)
	}

	return gr, nil
}
