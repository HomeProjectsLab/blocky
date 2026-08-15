package configstore

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"gorm.io/gorm"
)

// Household groups compose the existing blocking primitives: a group bundles a
// set of blocklist categories, and its members (client name, IP or CIDR) all
// reference that bundle. blocky's native ClientGroupsBlock/Denylists do the
// many-to-many work — see overlayBlocking. No resolver change, no per-request
// join. Group-scoped manual allow/deny reuse AllowlistEntry/DenylistEntry with
// GroupName == the group name.

// BlockingGroup is one named household group. Disabled groups drop out of the
// overlay entirely (their members fall back to the global categories).
type BlockingGroup struct {
	Name    string `gorm:"primaryKey"`
	Enabled bool
}

func (BlockingGroup) TableName() string { return "blocking_group" }

// BlockingGroupCategory assigns one category to one group. Group maps to the
// group_name column ("group" is a reserved SQL word).
type BlockingGroupCategory struct {
	ID       uint   `gorm:"primaryKey"`
	Group    string `gorm:"column:group_name;index"`
	Category string
}

func (BlockingGroupCategory) TableName() string { return "blocking_group_category" }

// BlockingGroupMember assigns one client (name, IP or CIDR) to one group.
type BlockingGroupMember struct {
	ID     uint   `gorm:"primaryKey"`
	Client string `gorm:"index"`
	Group  string `gorm:"column:group_name;index"`
}

func (BlockingGroupMember) TableName() string { return "blocking_group_member" }

// GroupView is one group with its categories and members, for the UI/API.
type GroupView struct {
	Name       string
	Enabled    bool
	Categories []string
	Members    []string
}

// ListGroups returns every group with its categories and members, name-ordered.
func (s *Store) ListGroups() ([]GroupView, error) {
	b, err := s.loadBlockingRows()
	if err != nil {
		return nil, err
	}

	cats := map[string][]string{}
	for _, gc := range b.groupCats {
		cats[gc.Group] = append(cats[gc.Group], gc.Category)
	}

	members := map[string][]string{}
	for _, m := range b.groupMembers {
		members[m.Group] = append(members[m.Group], m.Client)
	}

	out := make([]GroupView, 0, len(b.groups))
	for _, g := range b.groups {
		out = append(out, GroupView{
			Name: g.Name, Enabled: g.Enabled,
			Categories: cats[g.Name], Members: members[g.Name],
		})
	}

	return out, nil
}

// SaveGroup upserts a group and replaces its category set. Categories must be
// seeded names; a new group starts enabled. Validated before persist.
func (s *Store) SaveGroup(name string, categories []string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("group name is required")
	}

	b, err := s.loadBlockingRows()
	if err != nil {
		return err
	}

	for _, cat := range categories {
		if !slices.ContainsFunc(b.cats, func(c BlockingCategory) bool { return c.Name == cat }) {
			return fmt.Errorf("unknown blocklist category '%s'", cat)
		}
	}

	// candidate: keep existing Enabled (default true for a fresh group)
	enabled := true

	if i := slices.IndexFunc(b.groups, func(g BlockingGroup) bool { return g.Name == name }); i >= 0 {
		enabled = b.groups[i].Enabled
	} else {
		b.groups = append(b.groups, BlockingGroup{Name: name, Enabled: enabled})
	}

	b.groupCats = slices.DeleteFunc(b.groupCats, func(gc BlockingGroupCategory) bool { return gc.Group == name })
	for _, cat := range categories {
		b.groupCats = append(b.groupCats, BlockingGroupCategory{Group: name, Category: cat})
	}

	if err := s.validateBlockingCandidate(b); err != nil {
		return err
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&BlockingGroup{Name: name, Enabled: enabled}).Error; err != nil {
			return fmt.Errorf("can't persist group: %w", err)
		}

		if err := tx.Where("group_name = ?", name).Delete(&BlockingGroupCategory{}).Error; err != nil {
			return fmt.Errorf("can't clear group categories: %w", err)
		}

		for _, cat := range categories {
			if err := tx.Create(&BlockingGroupCategory{Group: name, Category: cat}).Error; err != nil {
				return fmt.Errorf("can't persist group category: %w", err)
			}
		}

		return nil
	})
}

// SetGroupMembers replaces one group's member set transactionally. Clients are
// matched by name, IP or CIDR (blocky's native semantics). Validated before persist.
func (s *Store) SetGroupMembers(group string, clients []string) error {
	group = strings.TrimSpace(group)
	if group == "" {
		return errors.New("group name is required")
	}

	b, err := s.loadBlockingRows()
	if err != nil {
		return err
	}

	if !slices.ContainsFunc(b.groups, func(g BlockingGroup) bool { return g.Name == group }) {
		return fmt.Errorf("unknown group '%s'", group)
	}

	cleaned := make([]string, 0, len(clients))

	for _, c := range clients {
		if c = strings.TrimSpace(c); c != "" {
			cleaned = append(cleaned, c)
		}
	}

	b.groupMembers = slices.DeleteFunc(b.groupMembers, func(m BlockingGroupMember) bool { return m.Group == group })
	for _, c := range cleaned {
		b.groupMembers = append(b.groupMembers, BlockingGroupMember{Client: c, Group: group})
	}

	if err := s.validateBlockingCandidate(b); err != nil {
		return err
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("group_name = ?", group).Delete(&BlockingGroupMember{}).Error; err != nil {
			return fmt.Errorf("can't clear group members: %w", err)
		}

		for _, c := range cleaned {
			if err := tx.Create(&BlockingGroupMember{Client: c, Group: group}).Error; err != nil {
				return fmt.Errorf("can't persist group member: %w", err)
			}
		}

		return nil
	})
}

// SetGroupEnabled toggles one group. Validated before persist.
func (s *Store) SetGroupEnabled(name string, enabled bool) error {
	b, err := s.loadBlockingRows()
	if err != nil {
		return err
	}

	i := slices.IndexFunc(b.groups, func(g BlockingGroup) bool { return g.Name == name })
	if i < 0 {
		return fmt.Errorf("unknown group '%s'", name)
	}

	b.groups[i].Enabled = enabled

	if err := s.validateBlockingCandidate(b); err != nil {
		return err
	}

	if err := s.db.Model(&BlockingGroup{}).Where("name = ?", name).Update("enabled", enabled).Error; err != nil {
		return fmt.Errorf("can't persist group: %w", err)
	}

	return nil
}

// DeleteGroup removes a group with its categories and members.
func (s *Store) DeleteGroup(name string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("group_name = ?", name).Delete(&BlockingGroupCategory{}).Error; err != nil {
			return fmt.Errorf("can't delete group categories: %w", err)
		}

		if err := tx.Where("group_name = ?", name).Delete(&BlockingGroupMember{}).Error; err != nil {
			return fmt.Errorf("can't delete group members: %w", err)
		}

		if err := tx.Where("name = ?", name).Delete(&BlockingGroup{}).Error; err != nil {
			return fmt.Errorf("can't delete group: %w", err)
		}

		return nil
	})
}
