package lists

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/log"
)

const (
	sourceTranco = "tranco"
	sourceBlp    = "blocklistproject"

	// httpTimeout bounds a single list fetch. The 1M Tranco download and the
	// multi-MB blocklist categories are the large cases.
	httpTimeout = 5 * time.Minute
)

// ListUpdateTotal counts updater checks by source and result. It is exported so
// the server can register it against blocky's metrics registry (lists cannot
// import metrics — metrics imports lists).
//
//nolint:gochecknoglobals
var ListUpdateTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "blocky_list_update_total",
	Help: "List updater checks by source and result (unchanged|replaced|error)",
}, []string{"source", "result"})

// HTTPGetFunc fetches a URL's body. Injected so unit tests never hit the
// network; production uses defaultGet.
type HTTPGetFunc func(ctx context.Context, url string) ([]byte, error)

// Store is the write side of the query-log list tables the updater drives.
// *querylog.DecoySource implements it; tests can pass a real temp-DB source.
type Store interface {
	GetListMeta(source, category string) (string, error)
	SetListMeta(source, category, version string) error
	ReplaceDecoy(r io.Reader) (int, error)
	SeedBlocklistIfEmpty(category string, r io.Reader) (int, error)
	ReplaceBlocklist(category string, r io.Reader) (int, error)
	PruneBlocklist(category string) error
}

// Updater keeps the Tranco decoy list and the blocklistproject category lists
// fresh in the query-log database. Embedded assets are the cold-start floor;
// upstream is only pulled when its version differs from what is stored.
type Updater struct {
	cfg          config.ListUpdaterConfig
	store        Store
	decoyEnabled bool // only refresh the decoy list when the decoy engine uses it
	get          HTTPGetFunc
	logger       *logrus.Entry
	// enabledCats reports which blocklist categories to seed/keep. nil => the
	// small default set. Injected by the server from the config store so a fresh
	// box only carries the enabled categories, not all ~5.4M embedded domains
	// (which is ~540MB in sqlite). Disabled categories are pruned on each seed.
	enabledCats func() ([]string, error)
}

// defaultSeedCategories mirrors configstore's default-on set; used when no
// enabled-categories provider is wired (e.g. tests, or no config store).
//
//nolint:gochecknoglobals
var defaultSeedCategories = []string{"ads", "tracking", "phishing"}

func NewUpdater(cfg config.ListUpdaterConfig, store Store, decoyEnabled bool) *Updater {
	return &Updater{
		cfg:          cfg,
		store:        store,
		decoyEnabled: decoyEnabled,
		get:          defaultGet,
		logger:       log.PrefixedLog("list-updater"),
	}
}

// SetEnabledCategories injects the provider for which blocklist categories to
// seed and keep. Call before Run.
func (u *Updater) SetEnabledCategories(fn func() ([]string, error)) {
	u.enabledCats = fn
}

// seedCategorySet returns the set of categories to seed; the rest are pruned.
func (u *Updater) seedCategorySet() (map[string]bool, error) {
	names := defaultSeedCategories

	if u.enabledCats != nil {
		got, err := u.enabledCats()
		if err != nil {
			return nil, err
		}

		names = got
	}

	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}

	return set, nil
}

// Run lays the embedded blocklist floor, runs one immediate version check, then
// re-checks on the configured interval until ctx is cancelled.
func (u *Updater) Run(ctx context.Context) {
	if err := u.SeedBlocklistFloor(); err != nil {
		u.logger.WithError(err).Error("can't seed embedded blocklist floor")
	}

	u.checkAll(ctx)

	ticker := time.NewTicker(time.Duration(u.cfg.IntervalHours) * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.checkAll(ctx)
		}
	}
}

// SeedBlocklistFloor seeds each embedded category into the DB when it has no
// rows, recording the embedded commit as the stored version so an identical
// upstream commit is a no-op (no needless first-launch re-download).
//
// ponytail: seeds ALL embedded categories (~5.4M rows, ~540MB sqlite, ~29s
// one-time on a laptop; malware 2.65M + porn 0.95M dominate). It runs in a
// background goroutine so it never blocks startup, and is version-gated so it
// only happens once. To shrink: drop a category's .txt.gz (a build-tag variant
// of blocklists.go) — EmbeddedCategories() then omits it everywhere — or let the
// ad-blocker restrict which categories it activates for blocking.
func (u *Updater) SeedBlocklistFloor() error {
	commit, err := EmbeddedCommit()
	if err != nil {
		return err
	}

	cats, err := EmbeddedCategories()
	if err != nil {
		return err
	}

	enabled, err := u.seedCategorySet()
	if err != nil {
		return err
	}

	for _, cat := range cats {
		// Only enabled categories are seeded; the rest are pruned so a fresh box
		// (or one where the user turned a giant like malware back off) doesn't
		// carry hundreds of MB of unused domains. Enabling a category triggers an
		// apply/rebuild, which re-runs this seeder and populates it.
		if !enabled[cat] {
			if err := u.store.PruneBlocklist(cat); err != nil {
				return fmt.Errorf("prune blocklist %q: %w", cat, err)
			}

			continue
		}

		r, err := OpenEmbeddedCategory(cat)
		if err != nil {
			return err
		}

		n, err := u.store.SeedBlocklistIfEmpty(cat, r)
		_ = r.Close()

		if err != nil {
			return fmt.Errorf("seed blocklist %q: %w", cat, err)
		}

		if n > 0 {
			u.logger.Infof("seeded %d %s domains from embedded floor", n, cat)
		}

		// record the embedded version if none stored yet (fresh seed or a
		// pre-existing table with no meta), so the version gate can short-circuit.
		if v, _ := u.store.GetListMeta(sourceBlp, cat); v == "" {
			if err := u.store.SetListMeta(sourceBlp, cat, commit); err != nil {
				return err
			}
		}
	}

	return nil
}

func (u *Updater) checkAll(ctx context.Context) {
	if u.decoyEnabled {
		u.checkTranco(ctx)
	}

	u.checkBlocklists(ctx)
}

// --- Tranco ----------------------------------------------------------------

func (u *Updater) checkTranco(ctx context.Context) {
	latest, err := u.latestTrancoID(ctx)
	if err != nil {
		u.logger.WithError(err).Warn("tranco version check failed")
		ListUpdateTotal.WithLabelValues(sourceTranco, "error").Inc()

		return
	}

	stored, err := u.store.GetListMeta(sourceTranco, "")
	if err != nil {
		u.logger.WithError(err).Warn("can't read tranco meta")
		ListUpdateTotal.WithLabelValues(sourceTranco, "error").Inc()

		return
	}

	if stored == latest {
		u.logger.Debugf("tranco up to date (%s)", latest)
		ListUpdateTotal.WithLabelValues(sourceTranco, "unchanged").Inc()

		return
	}

	u.logger.Infof("tranco list changed (%s -> %s); downloading", short(stored), latest)

	domains, err := u.downloadTranco(ctx, latest)
	if err != nil {
		u.logger.WithError(err).Warn("tranco download failed")
		ListUpdateTotal.WithLabelValues(sourceTranco, "error").Inc()

		return
	}

	n, err := u.store.ReplaceDecoy(bytes.NewReader(domains))
	if err != nil {
		u.logger.WithError(err).Warn("tranco repopulate failed (kept previous list)")
		ListUpdateTotal.WithLabelValues(sourceTranco, "error").Inc()

		return
	}

	if err := u.store.SetListMeta(sourceTranco, "", latest); err != nil {
		u.logger.WithError(err).Warn("can't update tranco meta")
	}

	u.logger.Infof("tranco decoy list replaced: %d domains (list %s)", n, latest)
	ListUpdateTotal.WithLabelValues(sourceTranco, "replaced").Inc()
}

func (u *Updater) latestTrancoID(ctx context.Context) (string, error) {
	body, err := u.get(ctx, u.cfg.TrancoURL+"/api/lists/date/latest")
	if err != nil {
		return "", err
	}

	var resp struct {
		ListID string `json:"list_id"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("can't parse tranco latest response: %w", err)
	}

	if resp.ListID == "" {
		return "", errors.New("tranco latest response has empty list_id")
	}

	return resp.ListID, nil
}

// downloadTranco fetches the top-1m zip and returns a newline-joined,
// normalized domain list (one per line).
func (u *Updater) downloadTranco(ctx context.Context, id string) ([]byte, error) {
	body, err := u.get(ctx, fmt.Sprintf("%s/download/%s/1000000", u.cfg.TrancoURL, id))
	if err != nil {
		return nil, err
	}

	return trancoZipToDomains(body)
}

// trancoZipToDomains extracts top-1m.csv ("rank,domain") from the zip and
// returns normalized "domain\n" lines.
func trancoZipToDomains(zipBytes []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, fmt.Errorf("tranco download is not a zip: %w", err)
	}

	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".csv") {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()

		var out bytes.Buffer
		if err := scanNormalized(rc, func(line string) {
			if i := strings.IndexByte(line, ','); i >= 0 {
				line = line[i+1:] // strip "rank," prefix
			}
			if d := normalizeDomain(line); d != "" {
				out.WriteString(d)
				out.WriteByte('\n')
			}
		}); err != nil {
			return nil, err
		}

		return out.Bytes(), nil
	}

	return nil, errors.New("no csv in tranco zip")
}

// --- blocklistproject ------------------------------------------------------

func (u *Updater) checkBlocklists(ctx context.Context) {
	latest, err := u.latestBlocklistCommit(ctx)
	if err != nil {
		u.logger.WithError(err).Warn("blocklistproject version check failed")
		ListUpdateTotal.WithLabelValues(sourceBlp, "error").Inc()

		return
	}

	cats, err := EmbeddedCategories()
	if err != nil {
		u.logger.WithError(err).Warn("can't list embedded categories")

		return
	}

	// Only refresh enabled categories. A disabled category has no seeded rows
	// (and no meta), so an unconditional check would treat "no stored version"
	// as "needs download" and pull every embedded category over the network —
	// re-seeding all ~5.4M domains regardless of what the user enabled.
	enabled, err := u.seedCategorySet()
	if err != nil {
		u.logger.WithError(err).Warn("can't determine enabled categories")

		return
	}

	changed := 0

	for _, cat := range cats {
		if !enabled[cat] {
			continue
		}

		stored, err := u.store.GetListMeta(sourceBlp, cat)
		if err != nil {
			u.logger.WithError(err).Warnf("can't read %s meta", cat)

			continue
		}

		if stored == latest {
			continue
		}

		if err := u.refreshBlocklistCategory(ctx, cat, latest); err != nil {
			u.logger.WithError(err).Warnf("refresh blocklist %q failed (kept previous)", cat)
			ListUpdateTotal.WithLabelValues(sourceBlp, "error").Inc()

			continue
		}
		changed++
	}

	if changed == 0 {
		u.logger.Debugf("blocklistproject up to date (%s)", short(latest))
		ListUpdateTotal.WithLabelValues(sourceBlp, "unchanged").Inc()
	}
}

func (u *Updater) refreshBlocklistCategory(ctx context.Context, cat, version string) error {
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/main/alt-version/%s-nl.txt",
		u.cfg.BlocklistRepo, cat)

	body, err := u.get(ctx, url)
	if err != nil {
		return err
	}

	var out bytes.Buffer
	if err := scanNormalized(bytes.NewReader(body), func(line string) {
		if d := normalizeDomain(line); d != "" {
			out.WriteString(d)
			out.WriteByte('\n')
		}
	}); err != nil {
		return err
	}

	n, err := u.store.ReplaceBlocklist(cat, &out)
	if err != nil {
		return err
	}

	if err := u.store.SetListMeta(sourceBlp, cat, version); err != nil {
		return err
	}

	u.logger.Infof("blocklist %q replaced: %d domains (commit %s)", cat, n, short(version))
	ListUpdateTotal.WithLabelValues(sourceBlp, "replaced").Inc()

	return nil
}

func (u *Updater) latestBlocklistCommit(ctx context.Context) (string, error) {
	body, err := u.get(ctx, fmt.Sprintf("https://api.github.com/repos/%s/commits/main", u.cfg.BlocklistRepo))
	if err != nil {
		return "", err
	}

	var resp struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("can't parse github commit response: %w", err)
	}

	if resp.SHA == "" {
		return "", errors.New("github commit response has empty sha")
	}

	return resp.SHA, nil
}

// --- helpers ---------------------------------------------------------------

// scanNormalized scans r line-by-line, stripping comment (#) content, and calls
// fn per raw line. Comment/blank filtering beyond '#' is left to normalizeDomain.
func scanNormalized(r io.Reader, fn func(line string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) //nolint:mnd
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fn(line)
	}

	return sc.Err()
}

// normalizeDomain lowercases/trims a domain and drops non-domain junk. Mirrors
// the generator so DB refresh and embedded floor agree byte-for-byte.
func normalizeDomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".")

	if s == "" || !strings.Contains(s, ".") || strings.ContainsAny(s, " \t") || strings.HasPrefix(s, ".") {
		return ""
	}

	return s
}

func short(v string) string {
	const n = 8
	if len(v) > n {
		return v[:n]
	}
	if v == "" {
		return "(none)"
	}

	return v
}

// defaultGet is the production fetcher: a context-bound GET with a bounded body.
func defaultGet(ctx context.Context, url string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)
	}

	return io.ReadAll(resp.Body)
}
