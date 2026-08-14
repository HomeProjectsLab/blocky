package querylog

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// blocklistDomain is one (category, domain) membership row. The composite
// primary key dedups within a category; a secondary index on domain makes the
// "which categories block X" lookup and the ad-blocker's per-domain checks fast.
type blocklistDomain struct {
	Category string `gorm:"column:category;primaryKey"`
	Domain   string `gorm:"column:domain;primaryKey;index:idx_blocklist_domain"`
}

func (blocklistDomain) TableName() string { return "blocklist_domains" }

// listMeta records, per (source, category), the upstream version the local copy
// was last populated from. It survives restart so the updater only re-fetches
// when the upstream version differs. source is "tranco" or "blocklistproject";
// category is "" for whole-list sources (tranco) or the category name.
type listMeta struct {
	Source    string    `gorm:"column:source;primaryKey"`
	Category  string    `gorm:"column:category;primaryKey"`
	Version   string    `gorm:"column:version"`
	FetchedAt time.Time `gorm:"column:fetched_at"`
}

func (listMeta) TableName() string { return "list_meta" }

// BlocklistStat is category metadata for the ad-blocker UI/source.
type BlocklistStat struct {
	Name  string `gorm:"column:category"`
	Count int64  `gorm:"column:n"`
}

// --- meta (version gate) ---------------------------------------------------

// GetListMeta returns the stored version for (source, category), or "" when
// nothing is recorded yet (cold start).
func (s *DecoySource) GetListMeta(source, category string) (string, error) {
	var m listMeta

	err := s.db.Where("source = ? AND category = ?", source, category).Take(&m).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}

		return "", err
	}

	return m.Version, nil
}

// BlocklistVersion returns the stored blocklistproject upstream version for a
// category (or "" when none recorded). The list loader folds it into a group's
// reuse fingerprint so an updater refresh (which bumps the version) busts only
// that category's cache — without lists having to import querylog.
func (s *DecoySource) BlocklistVersion(category string) (string, error) {
	return s.GetListMeta("blocklistproject", category)
}

// SetListMeta upserts the stored version for (source, category).
func (s *DecoySource) SetListMeta(source, category, version string) error {
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "source"}, {Name: "category"}},
		DoUpdates: clause.AssignmentColumns([]string{"version", "fetched_at"}),
	}).Create(&listMeta{Source: source, Category: category, Version: version, FetchedAt: time.Now()}).Error
}

// --- blocklist read (ad-blocker + noise) -----------------------------------

// BlocklistCategories returns each seeded category with its domain count.
func (s *DecoySource) BlocklistCategories() ([]BlocklistStat, error) {
	var out []BlocklistStat
	err := s.db.Raw(
		"SELECT category, COUNT(*) AS n FROM blocklist_domains GROUP BY category ORDER BY category").
		Scan(&out).Error

	return out, err
}

// BlocklistCount returns the number of domains in one category.
func (s *DecoySource) BlocklistCount(category string) (int64, error) {
	var n int64
	err := s.db.Raw("SELECT COUNT(*) FROM blocklist_domains WHERE category = ?", category).Scan(&n).Error

	return n, err
}

// ForEachBlocklistDomain streams every domain in a category to fn without
// materializing the whole (up to millions of rows) list. Stops and returns
// fn's error if it returns one. This is the ad-blocker's blocking-source read.
func (s *DecoySource) ForEachBlocklistDomain(category string, fn func(domain string) error) error {
	rows, err := s.db.Raw("SELECT domain FROM blocklist_domains WHERE category = ?", category).Rows()
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return err
		}

		if err := fn(d); err != nil {
			return err
		}
	}

	return rows.Err()
}

// SampleBlocklist returns a random domain across ALL blocklist categories by
// random rowid (gap-tolerant: takes the next existing row). Empty string when
// no blocklist is seeded. This is the noise machine's blocklist sampler.
//
// ponytail: per-category DELETE on refresh leaves rowid gaps, so a large
// contiguous gap biases sampling toward the row after it. Fine for noise;
// upgrade to a rowid remap or per-category max if uniformity ever matters.
func (s *DecoySource) SampleBlocklist() (string, error) {
	s.mu.Lock()
	max := s.blMaxRowid
	k := int64(0)
	if max > 0 {
		k = s.rnd.Int63n(max) + 1
	}
	s.mu.Unlock()

	if max == 0 {
		return "", nil
	}

	var domain string
	err := s.db.Raw("SELECT domain FROM blocklist_domains WHERE rowid >= ? ORDER BY rowid LIMIT 1", k).
		Scan(&domain).Error

	return domain, err
}

func (s *DecoySource) loadBlMaxRowid() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Raw("SELECT COALESCE(MAX(rowid),0) FROM blocklist_domains").Scan(&s.blMaxRowid).Error
}

// --- blocklist write (updater) ---------------------------------------------

// PruneBlocklist drops a category's rows and its version meta. Used by the
// floor seeder to reclaim space for categories that are not enabled — a fresh
// box only carries the enabled categories, not all ~5.4M embedded domains.
func (s *DecoySource) PruneBlocklist(category string) error {
	n, err := s.BlocklistCount(category)
	if err != nil {
		return err
	}

	if n == 0 {
		return nil // nothing seeded; leave meta untouched
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM blocklist_domains WHERE category = ?", category).Error; err != nil {
			return err
		}

		return tx.Exec("DELETE FROM list_meta WHERE source = ? AND category = ?", "blocklistproject", category).Error
	})
}

// SeedBlocklistIfEmpty inserts a category's domains only when that category has
// no rows. Returns rows inserted (0 when already present). Used to lay the
// embedded cold-start floor.
func (s *DecoySource) SeedBlocklistIfEmpty(category string, r io.Reader) (int, error) {
	n, err := s.BlocklistCount(category)
	if err != nil {
		return 0, err
	}

	if n > 0 {
		return 0, nil
	}

	return s.ReplaceBlocklist(category, r)
}

// ReplaceBlocklist atomically repopulates one category: DELETE + bulk insert in
// a single transaction, so a mid-stream failure rolls back and leaves the old
// rows intact (never an empty category). Returns rows inserted.
func (s *DecoySource) ReplaceBlocklist(category string, r io.Reader) (int, error) {
	inserted := 0

	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM blocklist_domains WHERE category = ?", category).Error; err != nil {
			return err
		}

		return streamInsert(r, func(batch []string) error {
			rows := make([]blocklistDomain, len(batch))
			for i, d := range batch {
				rows[i] = blocklistDomain{Category: category, Domain: d}
			}

			// ignore intra-category dup domains (composite PK conflict)
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error; err != nil {
				return fmt.Errorf("can't insert blocklist %q: %w", category, err)
			}
			inserted += len(rows)

			return nil
		})
	})
	if err != nil {
		return 0, err
	}

	return inserted, s.loadBlMaxRowid()
}

// ReplaceDecoy atomically repopulates the whole decoy_domains table (Tranco
// refresh): DELETE + bulk insert in one transaction. Rolls back on failure so
// the old list survives. Returns rows inserted and refreshes maxRowid.
func (s *DecoySource) ReplaceDecoy(r io.Reader) (int, error) {
	inserted := 0

	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM decoy_domains").Error; err != nil {
			return err
		}

		return streamInsert(r, func(batch []string) error {
			rows := make([]decoyDomain, len(batch))
			for i, d := range batch {
				rows[i] = decoyDomain{Domain: d}
			}

			if err := tx.Create(&rows).Error; err != nil {
				return fmt.Errorf("can't insert decoy domains: %w", err)
			}
			inserted += len(rows)

			return nil
		})
	})
	if err != nil {
		return 0, err
	}

	if err := s.loadMaxRowid(); err != nil {
		return inserted, err
	}

	return inserted, nil
}

// streamInsert scans r line-by-line (skipping blanks), buffering into batches of
// decoySeedBatch domains and calling flush for each. Keeps a multi-million-row
// list off the heap and out of one giant INSERT.
func streamInsert(r io.Reader, flush func(batch []string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) //nolint:mnd

	batch := make([]string, 0, decoySeedBatch)

	for scanner.Scan() {
		d := scanner.Text()
		if d == "" {
			continue
		}

		batch = append(batch, d)
		if len(batch) >= decoySeedBatch {
			if err := flush(batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	if len(batch) > 0 {
		return flush(batch)
	}

	return nil
}
