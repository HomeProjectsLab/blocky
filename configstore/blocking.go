package configstore

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/lists"
	"gorm.io/gorm"
)

// BlockingCategory is one preloaded blocklist category (blocklistproject).
// Enabled categories block for every client without a segment; segments pick
// their own subset. As soon as ANY category rows exist (they are seeded on
// Open), the blocking tables replace the YAML blob's denylists/allowlists/
// clientGroupsBlock entirely on load — same contract as the upstream tables.
type BlockingCategory struct {
	Name      string `gorm:"primaryKey"`
	Enabled   bool
	IsDefault bool
}

func (BlockingCategory) TableName() string { return "blocking_category" }

// BlockingClientSegment assigns one category to one client (name, IP or CIDR).
// A client with segment rows gets exactly those categories instead of the
// global enabled set (blocky's native client-group semantics).
type BlockingClientSegment struct {
	ID       uint   `gorm:"primaryKey"`
	Client   string `gorm:"index"`
	Category string
}

func (BlockingClientSegment) TableName() string { return "blocking_client_segment" }

// AllowlistEntry is one manual always-allow domain (group kept for future
// group-scoped lists; the UI uses "manual", which applies to every client).
// Enabled defaults true so rows predating the column read back active on upgrade.
type AllowlistEntry struct {
	ID        uint   `gorm:"primaryKey"`
	GroupName string `gorm:"index"`
	Domain    string
	Enabled   bool `gorm:"default:true"`
	Comment   string
}

func (AllowlistEntry) TableName() string { return "allowlist_entry" }

// DenylistEntry is one manual always-block domain (blocky syntax: plain
// domain, wildcard *.example.com, or /regex/).
type DenylistEntry struct {
	ID        uint   `gorm:"primaryKey"`
	GroupName string `gorm:"index"`
	Domain    string
	Enabled   bool `gorm:"default:true"`
	Comment   string
}

func (DenylistEntry) TableName() string { return "denylist_entry" }

// AdlistEntry is one blocklist URL (or local file path). Global — no group — it
// lives in its own "adlists" deny group, isolated from manual entries: mixing an
// HTTP source into "manual" would re-download every URL on each manual edit,
// because group cache reuse never reuses a group containing an HTTP source.
type AdlistEntry struct {
	ID      uint `gorm:"primaryKey"`
	URL     string
	Enabled bool `gorm:"default:true"`
	Comment string
}

func (AdlistEntry) TableName() string { return "adlist_entry" }

// defaultOnCategories are pre-enabled on first launch: the ad/privacy/fraud
// lists plus the small security ones. The giants (malware 2.65M, abuse 435k,
// porn 953k) and content filters (gambling, …) stay opt-in — enabling one
// seeds it on the next apply. This keeps a fresh box's DB ~100MB, not ~540MB.
//
//nolint:gochecknoglobals
var defaultOnCategories = map[string]bool{
	"ads": true, "tracking": true, "phishing": true,
	"scam": true, "ransomware": true, "fraud": true,
}

// seedBlockingCategories inserts one row per embedded blocklist category when
// the table is empty, pre-enabling the default set.
func seedBlockingCategories(db *gorm.DB) error {
	var count int64
	if err := db.Model(&BlockingCategory{}).Count(&count).Error; err != nil {
		return fmt.Errorf("can't read blocking categories: %w", err)
	}

	if count > 0 {
		return nil
	}

	cats, err := lists.EmbeddedCategories()
	if err != nil || len(cats) == 0 {
		// no embedded lists in this build: nothing to seed, blocking stays YAML-driven
		return nil //nolint:nilerr
	}

	rows := make([]BlockingCategory, 0, len(cats))
	for _, c := range cats {
		rows = append(rows, BlockingCategory{Name: c, Enabled: defaultOnCategories[c], IsDefault: defaultOnCategories[c]})
	}

	if err := db.Create(&rows).Error; err != nil {
		return fmt.Errorf("can't seed blocking categories: %w", err)
	}

	return nil
}

// blockingRows is a full snapshot of the blocking tables.
type blockingRows struct {
	cats         []BlockingCategory
	segs         []BlockingClientSegment
	allows       []AllowlistEntry
	denies       []DenylistEntry
	adlists      []AdlistEntry
	groups       []BlockingGroup
	groupCats    []BlockingGroupCategory
	groupMembers []BlockingGroupMember
}

// active reports whether the tables govern blocking (category rows exist).
func (b *blockingRows) active() bool { return len(b.cats) > 0 }

func (s *Store) loadBlockingRows() (*blockingRows, error) {
	var b blockingRows

	if err := s.conn().Order("name").Find(&b.cats).Error; err != nil {
		return nil, fmt.Errorf("can't read blocking categories: %w", err)
	}

	if err := s.conn().Order("client, category").Find(&b.segs).Error; err != nil {
		return nil, fmt.Errorf("can't read blocking segments: %w", err)
	}

	if err := s.conn().Order("id").Find(&b.allows).Error; err != nil {
		return nil, fmt.Errorf("can't read allowlist entries: %w", err)
	}

	if err := s.conn().Order("id").Find(&b.denies).Error; err != nil {
		return nil, fmt.Errorf("can't read denylist entries: %w", err)
	}

	if err := s.conn().Order("id").Find(&b.adlists).Error; err != nil {
		return nil, fmt.Errorf("can't read adlist entries: %w", err)
	}

	if err := s.conn().Order("name").Find(&b.groups).Error; err != nil {
		return nil, fmt.Errorf("can't read blocking groups: %w", err)
	}

	if err := s.conn().Order("group_name, category").Find(&b.groupCats).Error; err != nil {
		return nil, fmt.Errorf("can't read group categories: %w", err)
	}

	if err := s.conn().Order("group_name, client").Find(&b.groupMembers).Error; err != nil {
		return nil, fmt.Errorf("can't read group members: %w", err)
	}

	return &b, nil
}

// overlayBlocking replaces cfg's denylists/allowlists/clientGroupsBlock with
// the table state. Every enabled or segment-referenced category becomes a
// denylist group backed by a "blocklist:<cat>" source (streamed from the
// query-log DB by the list loader). Manual entries become inline text sources;
// their groups apply to every client (default and segmented alike).
func overlayBlocking(cfg *config.Config, b *blockingRows) {
	deny := map[string][]config.BytesSource{}
	allow := map[string][]config.BytesSource{}
	cgb := map[string][]string{}

	category := map[string]bool{}
	enabled := []string{}

	for _, c := range b.cats {
		category[c.Name] = true

		if c.Enabled {
			enabled = append(enabled, c.Name)
			deny[c.Name] = blocklistSource(c.Name)
		}
	}

	for _, seg := range b.segs {
		if deny[seg.Category] == nil {
			deny[seg.Category] = blocklistSource(seg.Category)
		}

		cgb[seg.Client] = append(cgb[seg.Client], seg.Category)
	}

	// manual entries, grouped
	denyByGroup := map[string][]string{}
	for _, e := range b.denies {
		if !e.Enabled {
			continue
		}

		denyByGroup[e.GroupName] = append(denyByGroup[e.GroupName], e.Domain)
	}

	allowByGroup := map[string][]string{}
	for _, e := range b.allows {
		if !e.Enabled {
			continue
		}

		allowByGroup[e.GroupName] = append(allowByGroup[e.GroupName], e.Domain)
	}

	manual := map[string]bool{}

	for g, ds := range denyByGroup {
		deny[g] = append(deny[g], config.TextBytesSource(ds...))
		manual[g] = true
	}

	for g, as := range allowByGroup {
		allow[g] = append(allow[g], config.TextBytesSource(as...))
		manual[g] = true

		// a group with only allowlist entries would flip blocky into
		// "allowlist-only" mode (block everything not listed); an empty deny
		// source keeps allow entries pure exceptions.
		if deny[g] == nil {
			deny[g] = []config.BytesSource{config.TextBytesSource()}
		}
	}

	// household groups: an enabled group unions its assigned categories into a
	// deny group named after it, and every member client references that group.
	// Group-scoped manual allow/deny (shared Allow/DenylistEntry.GroupName)
	// already populated deny[g]/allow[g] above and stay scoped to members here —
	// so a group name is NOT a global "manual" group.
	groupCats := map[string][]string{}
	for _, gc := range b.groupCats {
		groupCats[gc.Group] = append(groupCats[gc.Group], gc.Category)
	}

	membersByGroup := map[string][]string{}
	for _, m := range b.groupMembers {
		membersByGroup[m.Group] = append(membersByGroup[m.Group], m.Client)
	}

	isGroup := map[string]bool{}

	for _, grp := range b.groups {
		isGroup[grp.Name] = true

		if !grp.Enabled {
			continue
		}

		for _, cat := range groupCats[grp.Name] {
			deny[grp.Name] = append(deny[grp.Name], blocklistSource(cat)...)
		}

		members := membersByGroup[grp.Name]
		for _, client := range members {
			cgb[client] = append(cgb[client], grp.Name)
		}

		// an enabled group with members but no sources at all (no categories, no
		// group-scoped allow/deny) leaves deny[g] AND allow[g] nil while cgb
		// references it → cfg.Validate rejects it. Emit an empty deny source
		// (mirrors the allow-only guard above).
		if len(members) > 0 && deny[grp.Name] == nil && allow[grp.Name] == nil {
			deny[grp.Name] = []config.BytesSource{config.TextBytesSource()}
		}
	}

	// manual (non-category) groups apply to everyone: the default group and
	// every segmented client. Groups named after a category just extend that
	// category's sources and follow its activation instead. Household group
	// names are member-scoped (handled above), never global.
	defaults := enabled

	for g := range manual {
		if category[g] || isGroup[g] {
			continue
		}

		defaults = append(defaults, g)

		for client := range cgb {
			cgb[client] = append(cgb[client], g)
		}
	}

	// adlist URLs get their OWN "adlists" group (never mixed into "manual": a
	// manual edit must not re-download every URL). Like manual, it applies to
	// everyone — the default group and every segmented client.
	var urls []string

	for _, a := range b.adlists {
		if a.Enabled {
			urls = append(urls, a.URL)
		}
	}

	if len(urls) > 0 {
		deny["adlists"] = append(deny["adlists"], config.NewBytesSources(urls...)...)
		defaults = append(defaults, "adlists")

		for client := range cgb {
			cgb[client] = append(cgb[client], "adlists")
		}
	}

	if len(defaults) > 0 {
		slices.Sort(defaults)
		cgb["default"] = defaults
	}

	cfg.Blocking.Denylists = deny
	cfg.Blocking.Allowlists = allow
	cfg.Blocking.ClientGroupsBlock = cgb
}

func blocklistSource(category string) []config.BytesSource {
	return []config.BytesSource{{Type: config.BytesSourceTypeFile, From: lists.BlocklistSourcePrefix + category}}
}

// validateBlockingCandidate overlays the given table state onto the stored
// blob and runs full config validation. Nothing is persisted.
func (s *Store) validateBlockingCandidate(b *blockingRows) error {
	if !b.active() {
		return nil
	}

	blob, err := s.blob()
	if err != nil {
		return err
	}

	cfg, err := config.LoadFromYAML([]byte(blob.YAML))
	if err != nil {
		return err
	}

	overlayBlocking(cfg, b)

	return cfg.Validate()
}

// --- typed accessors ---------------------------------------------------------

// ListBlockingCategories returns all category rows, ordered by name.
func (s *Store) ListBlockingCategories() ([]BlockingCategory, error) {
	var cats []BlockingCategory
	if err := s.conn().Order("name").Find(&cats).Error; err != nil {
		return nil, fmt.Errorf("can't read blocking categories: %w", err)
	}

	return cats, nil
}

// SetCategoryEnabled toggles one seeded category. Unknown names are rejected.
func (s *Store) SetCategoryEnabled(name string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := s.loadBlockingRows()
	if err != nil {
		return err
	}

	i := slices.IndexFunc(b.cats, func(c BlockingCategory) bool { return c.Name == name })
	if i < 0 {
		return fmt.Errorf("unknown blocklist category '%s'", name)
	}

	b.cats[i].Enabled = enabled

	if err := s.validateBlockingCandidate(b); err != nil {
		return err
	}

	res := s.conn().Model(&BlockingCategory{}).Where("name = ?", name).Update("enabled", enabled)
	if res.Error != nil {
		return fmt.Errorf("can't persist blocking category: %w", res.Error)
	}

	return nil
}

// GetClientSegments returns the per-client category assignments.
func (s *Store) GetClientSegments() (map[string][]string, error) {
	var segs []BlockingClientSegment
	if err := s.conn().Order("client, category").Find(&segs).Error; err != nil {
		return nil, fmt.Errorf("can't read blocking segments: %w", err)
	}

	out := map[string][]string{}
	for _, seg := range segs {
		out[seg.Client] = append(out[seg.Client], seg.Category)
	}

	return out, nil
}

// SetClientSegment replaces one client's category set transactionally.
// An empty set removes the segment (the client falls back to the global
// enabled categories). Categories must be seeded names.
func (s *Store) SetClientSegment(client string, categories []string) error {
	client = strings.TrimSpace(client)
	if client == "" {
		return errors.New("client identifier is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := s.loadBlockingRows()
	if err != nil {
		return err
	}

	for _, cat := range categories {
		if !slices.ContainsFunc(b.cats, func(c BlockingCategory) bool { return c.Name == cat }) {
			return fmt.Errorf("unknown blocklist category '%s'", cat)
		}
	}

	b.segs = slices.DeleteFunc(b.segs, func(seg BlockingClientSegment) bool { return seg.Client == client })
	for _, cat := range categories {
		b.segs = append(b.segs, BlockingClientSegment{Client: client, Category: cat})
	}

	if err := s.validateBlockingCandidate(b); err != nil {
		return err
	}

	return s.conn().Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("client = ?", client).Delete(&BlockingClientSegment{}).Error; err != nil {
			return fmt.Errorf("can't clear blocking segment: %w", err)
		}

		for _, cat := range categories {
			if err := tx.Create(&BlockingClientSegment{Client: client, Category: cat}).Error; err != nil {
				return fmt.Errorf("can't persist blocking segment: %w", err)
			}
		}

		return nil
	})
}

// ListAllowEntries returns all manual allow entries, oldest first.
func (s *Store) ListAllowEntries() ([]AllowlistEntry, error) {
	var out []AllowlistEntry
	if err := s.conn().Order("id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("can't read allowlist entries: %w", err)
	}

	return out, nil
}

// ListDenyEntries returns all manual deny entries, oldest first.
func (s *Store) ListDenyEntries() ([]DenylistEntry, error) {
	var out []DenylistEntry
	if err := s.conn().Order("id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("can't read denylist entries: %w", err)
	}

	return out, nil
}

// validateListDomain accepts blocky list syntax: plain domain, *.wildcard or
// /regex/. It only rejects obvious garbage; the list parser is the authority.
func validateListDomain(domain string) (string, error) {
	domain = strings.TrimSpace(domain)
	if domain == "" || strings.ContainsAny(domain, " \t\n#") {
		return "", fmt.Errorf("invalid list entry %q", domain)
	}

	return domain, nil
}

// AddAllowEntry appends a manual allow entry and returns its id.
func (s *Store) AddAllowEntry(group, domain, comment string) (uint, error) {
	domain, err := validateListDomain(domain)
	if err != nil {
		return 0, err
	}

	if group == "" {
		group = "manual"
	}

	e := AllowlistEntry{GroupName: group, Domain: domain, Enabled: true, Comment: comment}
	if err := s.conn().Create(&e).Error; err != nil {
		return 0, fmt.Errorf("can't persist allowlist entry: %w", err)
	}

	return e.ID, nil
}

// AddDenyEntry appends a manual deny entry and returns its id.
func (s *Store) AddDenyEntry(group, domain, comment string) (uint, error) {
	domain, err := validateListDomain(domain)
	if err != nil {
		return 0, err
	}

	if group == "" {
		group = "manual"
	}

	e := DenylistEntry{GroupName: group, Domain: domain, Enabled: true, Comment: comment}
	if err := s.conn().Create(&e).Error; err != nil {
		return 0, fmt.Errorf("can't persist denylist entry: %w", err)
	}

	return e.ID, nil
}

// SetAllowEntry updates one manual allow entry's enabled flag and comment.
func (s *Store) SetAllowEntry(id uint, enabled bool, comment string) error {
	res := s.conn().Model(&AllowlistEntry{}).Where("id = ?", id).
		Updates(map[string]any{"enabled": enabled, "comment": comment})
	if res.Error != nil {
		return fmt.Errorf("can't persist allowlist entry: %w", res.Error)
	}

	return nil
}

// SetDenyEntry updates one manual deny entry's enabled flag and comment.
func (s *Store) SetDenyEntry(id uint, enabled bool, comment string) error {
	res := s.conn().Model(&DenylistEntry{}).Where("id = ?", id).
		Updates(map[string]any{"enabled": enabled, "comment": comment})
	if res.Error != nil {
		return fmt.Errorf("can't persist denylist entry: %w", res.Error)
	}

	return nil
}

// DeleteAllowEntry removes one manual allow entry by id.
func (s *Store) DeleteAllowEntry(id uint) error {
	return s.conn().Delete(&AllowlistEntry{}, id).Error
}

// DeleteDenyEntry removes one manual deny entry by id.
func (s *Store) DeleteDenyEntry(id uint) error {
	return s.conn().Delete(&DenylistEntry{}, id).Error
}

// --- adlist accessors --------------------------------------------------------

// ListAdlistEntries returns all blocklist URLs, oldest first.
func (s *Store) ListAdlistEntries() ([]AdlistEntry, error) {
	var out []AdlistEntry
	if err := s.conn().Order("id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("can't read adlist entries: %w", err)
	}

	return out, nil
}

// AddAdlistEntry appends a blocklist URL and returns its id. The candidate
// config is validated (through validateBlockingCandidate) before it is persisted.
func (s *Store) AddAdlistEntry(url, comment string) (uint, error) {
	url = strings.TrimSpace(url)
	if url == "" || strings.ContainsAny(url, " \t\n") {
		return 0, fmt.Errorf("invalid adlist URL %q", url)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := s.loadBlockingRows()
	if err != nil {
		return 0, err
	}

	e := AdlistEntry{URL: url, Enabled: true, Comment: comment}
	b.adlists = append(b.adlists, e)

	if err := s.validateBlockingCandidate(b); err != nil {
		return 0, err
	}

	if err := s.conn().Create(&e).Error; err != nil {
		return 0, fmt.Errorf("can't persist adlist entry: %w", err)
	}

	return e.ID, nil
}

// SetAdlistEnabled toggles one blocklist URL. Validated before persist.
func (s *Store) SetAdlistEnabled(id uint, enabled bool) error {
	return s.updateAdlist(id, map[string]any{"enabled": enabled}, func(e *AdlistEntry) { e.Enabled = enabled })
}

// UpdateAdlistEntry replaces one blocklist URL's url and comment. Validated
// before persist.
func (s *Store) UpdateAdlistEntry(id uint, url, comment string) error {
	url = strings.TrimSpace(url)
	if url == "" || strings.ContainsAny(url, " \t\n") {
		return fmt.Errorf("invalid adlist URL %q", url)
	}

	return s.updateAdlist(id, map[string]any{"url": url, "comment": comment}, func(e *AdlistEntry) {
		e.URL, e.Comment = url, comment
	})
}

// updateAdlist applies mutate to the in-memory row, validates the resulting
// config, then persists cols for the given id.
func (s *Store) updateAdlist(id uint, cols map[string]any, mutate func(*AdlistEntry)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := s.loadBlockingRows()
	if err != nil {
		return err
	}

	i := slices.IndexFunc(b.adlists, func(a AdlistEntry) bool { return a.ID == id })
	if i < 0 {
		return fmt.Errorf("unknown adlist entry %d", id)
	}

	mutate(&b.adlists[i])

	if err := s.validateBlockingCandidate(b); err != nil {
		return err
	}

	if err := s.conn().Model(&AdlistEntry{}).Where("id = ?", id).Updates(cols).Error; err != nil {
		return fmt.Errorf("can't persist adlist entry: %w", err)
	}

	return nil
}

// DeleteAdlistEntry removes one blocklist URL by id.
func (s *Store) DeleteAdlistEntry(id uint) error {
	return s.conn().Delete(&AdlistEntry{}, id).Error
}
