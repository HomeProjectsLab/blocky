package configstore

import (
	"errors"
	"fmt"
	"time"

	"github.com/0xERR0R/blocky/config"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UpstreamGroup is one upstream group row. As soon as ANY group rows exist,
// they replace the YAML blob's upstreams.groups and upstreams.groupConfig
// entirely on load — the blob's upstreams section is then ignored (and shown
// stale in the raw editor; acceptable until Phase 4 reworks the seed).
type UpstreamGroup struct {
	Name      string `gorm:"primaryKey"`
	Strategy  string // "" or "parallel_best" = fall back to the global strategy
	HopMin    int64  // nanoseconds; 0 = default (time_hop only)
	HopMax    int64  // nanoseconds; 0 = default (time_hop only)
	UpdatedAt time.Time
}

func (UpstreamGroup) TableName() string { return "upstream_group" }

// UpstreamEntry is one upstream address within a group. Address uses the
// same string format as YAML upstreams ([net]:host[:port][/path][#cn]).
type UpstreamEntry struct {
	ID        uint   `gorm:"primaryKey"`
	GroupName string `gorm:"index"`
	Address   string
	Weight    uint
	// no gorm default: a DB default would make gorm skip explicit false on
	// insert. "absent means enabled" is handled at the API layer instead.
	Enabled  bool
	Position int
}

func (UpstreamEntry) TableName() string { return "upstream_entry" }

// ListUpstreamGroups returns all group rows (ordered by name) and their
// entries (ordered by position) keyed by group name.
func (s *Store) ListUpstreamGroups() ([]UpstreamGroup, map[string][]UpstreamEntry, error) {
	var groups []UpstreamGroup
	if err := s.conn().Order("name").Find(&groups).Error; err != nil {
		return nil, nil, fmt.Errorf("can't read upstream groups: %w", err)
	}

	var entries []UpstreamEntry
	if err := s.conn().Order("group_name, position, id").Find(&entries).Error; err != nil {
		return nil, nil, fmt.Errorf("can't read upstream entries: %w", err)
	}

	byGroup := make(map[string][]UpstreamEntry, len(groups))
	for _, e := range entries {
		byGroup[e.GroupName] = append(byGroup[e.GroupName], e)
	}

	return groups, byGroup, nil
}

// PutUpstreamGroup validates the whole config with the group upserted and
// persists it on success.
func (s *Store) PutUpstreamGroup(g UpstreamGroup) error {
	if g.Name == "" {
		return errors.New("upstream group name is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	groups, entries, err := s.ListUpstreamGroups()
	if err != nil {
		return err
	}

	replaced := false

	for i := range groups {
		if groups[i].Name == g.Name {
			groups[i] = g
			replaced = true
		}
	}

	if !replaced {
		groups = append(groups, g)
	}

	if err := s.validateCandidate(groups, entries); err != nil {
		return err
	}

	if err := s.conn().Clauses(clause.OnConflict{UpdateAll: true}).Create(&g).Error; err != nil {
		return fmt.Errorf("can't persist upstream group: %w", err)
	}

	return nil
}

// DeleteUpstreamGroup removes a group and its entries. The default group
// can't be deleted.
func (s *Store) DeleteUpstreamGroup(name string) error {
	if name == config.UpstreamDefaultCfgName {
		return errors.New("the default upstream group can't be deleted")
	}

	// s.mu: every mutator must serialize against SnapshotTo/RestoreDB
	// (see store.go SnapshotTo) like the other writers.
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.conn().Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("group_name = ?", name).Delete(&UpstreamEntry{}).Error; err != nil {
			return fmt.Errorf("can't delete upstream entries: %w", err)
		}

		res := tx.Delete(&UpstreamGroup{Name: name})
		if res.Error != nil {
			return fmt.Errorf("can't delete upstream group: %w", res.Error)
		}

		if res.RowsAffected == 0 {
			return fmt.Errorf("unknown upstream group '%s'", name)
		}

		return nil
	})
}

// SetUpstreamEntries replaces the full entry list of a group transactionally.
// Positions are normalized to the given order; IDs are reassigned. The whole
// candidate config is validated before anything is written.
func (s *Store) SetUpstreamEntries(group string, entries []UpstreamEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	groups, byGroup, err := s.ListUpstreamGroups()
	if err != nil {
		return err
	}

	known := false

	for _, g := range groups {
		if g.Name == group {
			known = true

			break
		}
	}

	if !known {
		return fmt.Errorf("unknown upstream group '%s'", group)
	}

	for i := range entries {
		// parse every address (even disabled ones) so garbage never lands in the DB
		if _, err := config.ParseUpstream(entries[i].Address); err != nil {
			return err
		}

		entries[i].ID = 0
		entries[i].GroupName = group
		entries[i].Position = i
	}

	byGroup[group] = entries

	if err := s.validateCandidate(groups, byGroup); err != nil {
		return err
	}

	return s.conn().Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("group_name = ?", group).Delete(&UpstreamEntry{}).Error; err != nil {
			return fmt.Errorf("can't clear upstream entries: %w", err)
		}

		if len(entries) == 0 {
			return nil
		}

		if err := tx.Create(&entries).Error; err != nil {
			return fmt.Errorf("can't persist upstream entries: %w", err)
		}

		return nil
	})
}

// UpstreamsFromEntries converts stored rows to config.Upstream values,
// skipping disabled entries. Used by the overlay and the live-swap API.
func UpstreamsFromEntries(entries []UpstreamEntry) ([]config.Upstream, error) {
	ups := make([]config.Upstream, 0, len(entries))

	for _, e := range entries {
		if !e.Enabled {
			continue
		}

		u, err := config.ParseUpstream(e.Address)
		if err != nil {
			return nil, fmt.Errorf("upstream entry '%s': %w", e.Address, err)
		}

		u.Weight = e.Weight
		ups = append(ups, u)
	}

	return ups, nil
}

// overlayUpstreams replaces cfg's upstream groups and group config with the
// table contents. Groups may have zero (enabled) entries, e.g. recursive ones.
func overlayUpstreams(cfg *config.Config, groups []UpstreamGroup, entries map[string][]UpstreamEntry) error {
	cfg.Upstreams.Groups = make(config.UpstreamGroups, len(groups))
	cfg.Upstreams.GroupConfig = make(map[string]config.UpstreamGroupConfig, len(groups))

	for _, g := range groups {
		gc := config.UpstreamGroupConfig{
			HopMin: config.Duration(g.HopMin),
			HopMax: config.Duration(g.HopMax),
		}

		if g.Strategy != "" {
			strategy, err := config.ParseUpstreamStrategy(g.Strategy)
			if err != nil {
				return fmt.Errorf("upstream group '%s': %w", g.Name, err)
			}

			gc.Strategy = strategy
		}

		cfg.Upstreams.GroupConfig[g.Name] = gc

		ups, err := UpstreamsFromEntries(entries[g.Name])
		if err != nil {
			return fmt.Errorf("upstream group '%s': %w", g.Name, err)
		}

		cfg.Upstreams.Groups[g.Name] = ups
	}

	return nil
}

// validateCandidate builds the config the overlay would produce from the
// given tables state and runs full validation on it. Nothing is persisted.
func (s *Store) validateCandidate(groups []UpstreamGroup, entries map[string][]UpstreamEntry) error {
	if len(groups) == 0 {
		return nil
	}

	b, err := s.blob()
	if err != nil {
		return err
	}

	cfg, err := config.LoadFromYAML([]byte(b.YAML))
	if err != nil {
		return err
	}

	if err := overlayUpstreams(cfg, groups, entries); err != nil {
		return err
	}

	return cfg.Validate()
}
