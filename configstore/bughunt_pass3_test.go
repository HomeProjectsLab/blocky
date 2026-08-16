package configstore

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/lists"
)

// Restoring a backup swaps the whole file without touching the overlay-table
// dirty hooks; Status() must still report dirty afterwards.
func TestRestoreDBMarksDirty(t *testing.T) {
	dir := t.TempDir()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	snap := filepath.Join(dir, "snap.db")
	if err := s.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}

	s.MarkApplied()

	if dirty, _, _, err := s.Status(); err != nil || dirty {
		t.Fatalf("want clean after apply, got dirty=%v err=%v", dirty, err)
	}

	if err := s.RestoreDB(snap); err != nil {
		t.Fatal(err)
	}

	if dirty, _, _, err := s.Status(); err != nil || !dirty {
		t.Fatalf("want dirty after restore, got dirty=%v err=%v", dirty, err)
	}
}

// The conditional writers must touch ONLY conditional.mapping, preserving
// comments in the hand-editable blob (yaml.v3 node surgery, not a map round-trip).
func TestConditionalPreservesComments(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	raw, err := s.RawYAML()
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SetRawYAML("# keep-me\n" + raw); err != nil {
		t.Fatal(err)
	}

	if err := s.SetConditionalMapping("corp.example", []string{"10.0.0.1", "10.0.0.2"}); err != nil {
		t.Fatal(err)
	}

	after, err := s.RawYAML()
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(after, "# keep-me") {
		t.Fatalf("comment flattened by conditional write:\n%s", after)
	}

	m, err := s.GetConditional()
	if err != nil {
		t.Fatal(err)
	}

	if len(m["corp.example"]) != 2 {
		t.Fatalf("mapping not written: %v", m)
	}

	if err := s.DeleteConditionalMapping("corp.example"); err != nil {
		t.Fatal(err)
	}

	if m, _ := s.GetConditional(); len(m["corp.example"]) != 0 {
		t.Fatalf("mapping not deleted: %v", m)
	}
}

// Open must reconcile blocking_category with the embedded set: a category
// added by an upgrade is inserted, a removed one is deleted (with its segment
// references), so no dangling "blocklist:<cat>" source is ever emitted.
func TestCategoryReconcileOnOpen(t *testing.T) {
	cats, err := lists.EmbeddedCategories()
	if err != nil || len(cats) == 0 {
		t.Skip("no embedded lists in this build")
	}

	dir := t.TempDir()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	victim := cats[0]

	// simulate a pre-upgrade DB: one embedded category missing, one stale row
	if err := s.conn().Where("name = ?", victim).Delete(&BlockingCategory{}).Error; err != nil {
		t.Fatal(err)
	}

	if err := s.conn().Create(&BlockingCategory{Name: "obsolete-cat", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}

	if err := s.conn().Create(&BlockingClientSegment{Client: "10.0.0.9", Category: "obsolete-cat"}).Error; err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rows, err := s.ListBlockingCategories()
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	for _, c := range rows {
		names[c.Name] = true
	}

	if !names[victim] {
		t.Fatalf("missing embedded category %q not re-inserted", victim)
	}

	if names["obsolete-cat"] {
		t.Fatal("stale category row survived reconcile")
	}

	if segs, _ := s.GetClientSegments(); len(segs["10.0.0.9"]) != 0 {
		t.Fatalf("stale segment survived reconcile: %v", segs)
	}
}

// An enabled group with zero members is referenced by nobody; its category
// blocklists must NOT be emitted (each would load a full trie into RAM).
func TestOverlayBlockingSkipsMemberlessGroup(t *testing.T) {
	b := &blockingRows{
		cats:      []BlockingCategory{{Name: "ads"}},
		groups:    []BlockingGroup{{Name: "kids", Enabled: true}},
		groupCats: []BlockingGroupCategory{{Group: "kids", Category: "ads"}},
	}

	var cfg config.Config

	overlayBlocking(&cfg, b)

	if _, ok := cfg.Blocking.Denylists["kids"]; ok {
		t.Fatal("memberless group emitted deny sources")
	}

	b.groupMembers = []BlockingGroupMember{{Group: "kids", Client: "10.0.0.9"}}

	var cfg2 config.Config

	overlayBlocking(&cfg2, b)

	if _, ok := cfg2.Blocking.Denylists["kids"]; !ok {
		t.Fatal("group with a member lost its deny sources")
	}
}

// Reserved-name and unknown-target guards added in pass 3.
func TestPass3Guards(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.SetClientSegment("default", nil); err == nil {
		t.Fatal("client name 'default' accepted; overlay would overwrite it")
	}

	if _, err := s.AddDenyEntry("adlists", "ads.example.com", ""); err == nil {
		t.Fatal("deny entry accepted into reserved 'adlists' group")
	}

	if _, err := s.AddAllowEntry("adlists", "ok.example.com", ""); err == nil {
		t.Fatal("allow entry accepted into reserved 'adlists' group")
	}

	if err := s.DeleteUpstreamGroup("no-such-group"); err == nil {
		t.Fatal("deleting a nonexistent upstream group reported success")
	}
}
