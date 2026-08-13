package lists

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// BlocklistSourcePrefix marks a BytesSource that reads a blocklist category
// from the query-log database instead of a file/URL: "blocklist:ads".
// A bare "blocklist:ads" string parses as a file BytesSource, so no config
// changes are needed — NewSourceOpener special-cases the prefix.
const BlocklistSourcePrefix = "blocklist:"

// BlocklistProvider streams the domains of one seeded blocklist category.
// *querylog.DecoySource implements it; the server registers it at startup
// (lists cannot import querylog — metrics→lists would cycle).
type BlocklistProvider interface {
	ForEachBlocklistDomain(category string, fn func(domain string) error) error
}

//nolint:gochecknoglobals // process-wide registry, set once by the server before resolvers are built
var (
	blProviderMu sync.RWMutex
	blProvider   BlocklistProvider
)

// SetBlocklistProvider registers the database-backed category source. Must be
// called before a BlockingResolver referencing "blocklist:" sources loads.
func SetBlocklistProvider(p BlocklistProvider) {
	blProviderMu.Lock()
	defer blProviderMu.Unlock()

	blProvider = p
}

func blocklistProvider() BlocklistProvider {
	blProviderMu.RLock()
	defer blProviderMu.RUnlock()

	return blProvider
}

// blocklistOpener adapts one category of the blocklist_domains table to the
// SourceOpener interface the list loader consumes. Each Open streams the
// current table contents, so a periodic list refresh picks up updater changes.
type blocklistOpener struct {
	category string
}

func (o *blocklistOpener) Open(_ context.Context) (io.ReadCloser, error) {
	p := blocklistProvider()
	if p == nil {
		return nil, fmt.Errorf("blocklist source %q: no provider registered (query log not in sqlite mode?)", o.category)
	}

	pr, pw := io.Pipe()

	go func() {
		bw := bufio.NewWriterSize(pw, 64*1024) //nolint:mnd

		err := p.ForEachBlocklistDomain(o.category, func(domain string) error {
			_, werr := bw.WriteString(domain + "\n")

			return werr
		})
		if err == nil {
			err = bw.Flush()
		}

		// CloseWithError(nil) == Close; an early reader Close surfaces here as
		// ErrClosedPipe and stops the row scan via the callback error.
		_ = pw.CloseWithError(err)
	}()

	return pr, nil
}

func (o *blocklistOpener) String() string {
	return BlocklistSourcePrefix + o.category
}

func newBlocklistOpener(from string) *blocklistOpener {
	return &blocklistOpener{category: strings.TrimPrefix(from, BlocklistSourcePrefix)}
}
