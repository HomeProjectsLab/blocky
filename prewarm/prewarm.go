// Package prewarm proactively seeds the noise corpus with trending/rising
// domains BEFORE the user first visits them, closing the first-visit residual:
// a genuinely new domain the user hits is already covered by chaff.
//
// Source (offline-first, no credentials): with no configured URL it mines the
// embedded Tranco list's mid-popularity band (ranks ~bandLo-bandHi — the sites a
// normal person is likely to newly encounter; the head is already always-on
// noise, the deep tail nobody visits), rotating the slab each run so the corpus
// broadens over time. Set privacy.decoy.prewarmURL to a trending feed instead
// (Tranco "rising" or a Cloudflare Radar CSV/txt export — both plain
// rank,domain or domain-per-line, no auth needed) and it fetches that.
package prewarm

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/decoy"
	"github.com/0xERR0R/blocky/log"
)

const (
	// Mid-popularity band of the embedded Tranco list (rank order). Below bandLo
	// is head traffic already covered by replay/list noise; above bandHi is a tail
	// a normal user rarely encounters.
	bandLo = 1000
	bandHi = 50000

	defaultPerRun   = 500
	defaultInterval = 12 * time.Hour
	httpTimeout     = 30 * time.Second
	maxBody         = 32 << 20 // 32MiB cap on a fetched trending feed
)

// HTTPGetFunc fetches a URL's body. Injected so unit tests never hit the network.
type HTTPGetFunc func(ctx context.Context, url string) ([]byte, error)

// CorpusAdder is the write side the worker drives: *querylog.DecoySource
// implements it (AddToCorpus). Tests can pass a fake with no sqlite.
type CorpusAdder interface {
	AddToCorpus(domain string) error
}

// Worker pulls a bounded batch of trending/band domains into the corpus on an
// interval. Single-goroutine (Run); no locking needed on offset.
type Worker struct {
	src      CorpusAdder
	url      string
	interval time.Duration
	perRun   int
	get      HTTPGetFunc
	logger   *logrus.Entry
	offset   int // rotating cursor into the embedded band
}

// New builds the worker from decoy config, or returns nil when pre-warming is
// disabled or there is no corpus to write to (non-sqlite). Nil is safe to Run.
func New(cfg config.DecoyConfig, src CorpusAdder) *Worker {
	if !cfg.PrewarmEnable || src == nil {
		return nil
	}

	interval := time.Duration(cfg.PrewarmIntervalHours) * time.Hour
	if interval <= 0 {
		interval = defaultInterval
	}

	return &Worker{
		src:      src,
		url:      cfg.PrewarmURL,
		interval: interval,
		perRun:   defaultPerRun,
		get:      defaultGet,
		logger:   log.PrefixedLog("prewarm"),
	}
}

// Run warms once immediately, then on each interval tick until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	if w == nil {
		return
	}

	w.tick(ctx)

	t := time.NewTicker(w.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// tick fetches one batch and adds each domain to the corpus (bounded by perRun).
func (w *Worker) tick(ctx context.Context) {
	domains, err := w.fetch(ctx)
	if err != nil {
		w.logger.WithError(err).Warn("prewarm fetch failed")

		return
	}

	n := 0
	for _, d := range domains {
		if n >= w.perRun {
			break
		}
		if d == "" {
			continue
		}
		if err := w.src.AddToCorpus(d); err != nil {
			w.logger.WithError(err).Debug("prewarm add failed")

			continue
		}
		n++
	}

	w.logger.Infof("pre-warmed %d domains into noise corpus", n)
}

// fetch returns this run's domains: the configured trending feed if a URL is
// set, otherwise the next rotating slab of the embedded Tranco mid-band.
func (w *Worker) fetch(ctx context.Context) ([]string, error) {
	if w.url != "" {
		body, err := w.get(ctx, w.url)
		if err != nil {
			return nil, err
		}

		return parseDomains(body, w.perRun), nil
	}

	return w.embeddedBand()
}

// embeddedBand reads the mid-popularity band once and returns the next perRun
// slab, advancing (and wrapping) the rotating cursor.
func (w *Worker) embeddedBand() ([]string, error) {
	r, err := decoy.OpenList()
	if err != nil {
		return nil, err
	}
	defer r.Close()

	band := make([]string, 0, bandHi-bandLo)
	sc := bufio.NewScanner(r)
	rank := 0
	for sc.Scan() {
		rank++
		if rank < bandLo {
			continue
		}
		if rank >= bandHi {
			break
		}
		if d := strings.TrimSpace(sc.Text()); d != "" {
			band = append(band, d)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	if len(band) == 0 {
		return nil, nil
	}

	if w.offset >= len(band) {
		w.offset = 0
	}
	end := min(w.offset+w.perRun, len(band))
	slab := band[w.offset:end]
	w.offset = end

	return slab, nil
}

// parseDomains reads up to max domains from a trending feed. Accepts both
// domain-per-line and "rank,domain" CSV (Tranco / CF Radar); skips blanks and
// comments.
func parseDomains(body []byte, max int) []string {
	out := make([]string, 0, max)
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() && len(out) < max {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.LastIndex(line, ","); i >= 0 {
			line = strings.TrimSpace(line[i+1:])
		}
		if line != "" {
			out = append(out, line)
		}
	}

	return out
}

func defaultGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prewarm: GET %s: %s", url, resp.Status)
	}

	return io.ReadAll(io.LimitReader(resp.Body, maxBody))
}
