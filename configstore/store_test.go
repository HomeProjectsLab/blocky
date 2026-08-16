package configstore

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/0xERR0R/blocky/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestSnapshotRestoreBackupRoundTrip guards the overlay-leak: config lives in
// the YAML blob AND the upstream/denylist overlay tables, so a YAML-only backup
// would drop the table state. Seeding all three, snapshotting the whole file and
// restoring it into a fresh store must reproduce the exact same LoadConfig.
//
// Named for all three self-check filters (Snapshot/Restore/Backup) so `go test
// -run <any of them>` selects this plain test even though the suite is Ginkgo.
func TestSnapshotRestoreBackupRoundTrip(t *testing.T) {
	src, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}

	// YAML blob edit
	if err := src.SetRawYAML("ports:\n  http: 5353\n"); err != nil {
		t.Fatalf("set raw yaml: %v", err)
	}

	// upstream overlay tables: a default group row + entries
	if err := src.PutUpstreamGroup(UpstreamGroup{Name: "default", Strategy: "recursive"}); err != nil {
		t.Fatalf("put upstream group: %v", err)
	}

	if err := src.SetUpstreamEntries("default", []UpstreamEntry{
		{Address: "1.1.1.1", Enabled: true},
		{Address: "8.8.8.8", Enabled: true},
	}); err != nil {
		t.Fatalf("set upstream entries: %v", err)
	}

	// blocking overlay table: a manual denylist entry
	if _, err := src.AddDenyEntry("manual", "ads.example.com", ""); err != nil {
		t.Fatalf("add deny entry: %v", err)
	}

	want, err := src.LoadConfig()
	if err != nil {
		t.Fatalf("load source config: %v", err)
	}

	snap := filepath.Join(t.TempDir(), "backup.db")
	if err := src.SnapshotTo(snap); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	if err := src.Close(); err != nil {
		t.Fatalf("close source: %v", err)
	}

	// fresh store (its own seeded config.db), then restore the snapshot over it
	dst, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open dest store: %v", err)
	}
	defer dst.Close()

	if err := dst.RestoreDB(snap); err != nil {
		t.Fatalf("restore: %v", err)
	}

	got, err := dst.LoadConfig()
	if err != nil {
		t.Fatalf("load restored config: %v", err)
	}

	if !reflect.DeepEqual(want, got) {
		t.Fatalf("restored config differs from source:\nwant %+v\ngot  %+v", want, got)
	}
}

var _ = Describe("Store", func() {
	var (
		dir   string
		store *Store
	)

	BeforeEach(func() {
		dir = GinkgoT().TempDir()

		var err error
		store, err = Open(dir)
		Expect(err).Should(Succeed())

		DeferCleanup(func() {
			Expect(store.Close()).Should(Succeed())
		})
	})

	Describe("Open", func() {
		It("creates config.db and seeds the starter config", func() {
			_, err := os.Stat(filepath.Join(dir, "config.db"))
			Expect(err).Should(Succeed())

			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())

			Expect(cfg.Ports.HTTP).Should(ContainElement(":4000"))

			group := cfg.Upstreams.Groups["default"]
			Expect(group).Should(HaveLen(2))
			Expect(group[0].Host).Should(Equal("9.9.9.9"))
			Expect(group[1].Host).Should(Equal("149.112.112.112"))

			Expect(cfg.Upstreams.EffectiveStrategy("default")).
				Should(Equal(config.UpstreamStrategyRecursive))

			Expect(cfg.QueryLog.Type).Should(Equal(config.QueryLogTypeSqlite))
			Expect(string(cfg.QueryLog.Target)).Should(Equal(filepath.Join(dir, "querylog.db")))
		})

		It("does not re-seed on second Open", func() {
			modified := "ports:\n  http: 5353\n"
			Expect(store.SetRawYAML(modified)).Should(Succeed())
			Expect(store.Close()).Should(Succeed())

			var err error
			store, err = Open(dir)
			Expect(err).Should(Succeed())

			raw, err := store.RawYAML()
			Expect(err).Should(Succeed())
			Expect(raw).Should(Equal(modified))
		})
	})

	Describe("SetRawYAML", func() {
		It("rejects invalid YAML and keeps the stored blob", func() {
			before, err := store.RawYAML()
			Expect(err).Should(Succeed())

			Expect(store.SetRawYAML("notARealKey: true\n")).ShouldNot(Succeed())

			after, err := store.RawYAML()
			Expect(err).Should(Succeed())
			Expect(after).Should(Equal(before))
		})

		It("persists valid YAML and LoadConfig reflects it", func() {
			Expect(store.SetRawYAML("ports:\n  http: 5353\n")).Should(Succeed())

			cfg, err := store.LoadConfig()
			Expect(err).Should(Succeed())
			Expect(cfg.Ports.HTTP).Should(ContainElement(":5353"))
		})
	})

	Describe("ValidateRaw", func() {
		It("validates without persisting", func() {
			Expect(store.ValidateRaw("ports:\n  http: 5353\n")).Should(Succeed())
			Expect(store.ValidateRaw("notARealKey: true\n")).ShouldNot(Succeed())

			raw, err := store.RawYAML()
			Expect(err).Should(Succeed())
			Expect(raw).ShouldNot(ContainSubstring("5353"))
		})
	})

	Describe("RequestApply", func() {
		It("signals ApplyRequested and does not block when called twice", func() {
			store.RequestApply()
			store.RequestApply() // must not block

			Expect(store.ApplyRequested()).Should(Receive())
			Expect(store.ApplyRequested()).ShouldNot(Receive())
		})
	})

	Describe("Status", func() {
		It("is dirty before first apply, clean after MarkApplied, dirty again after SetRawYAML", func() {
			dirty, lastApplied, updatedAt, err := store.Status()
			Expect(err).Should(Succeed())
			Expect(dirty).Should(BeTrue())
			Expect(lastApplied.IsZero()).Should(BeTrue())
			Expect(updatedAt.IsZero()).Should(BeFalse())

			store.MarkApplied()

			dirty, lastApplied, _, err = store.Status()
			Expect(err).Should(Succeed())
			Expect(dirty).Should(BeFalse())
			Expect(lastApplied.IsZero()).Should(BeFalse())

			// ensure updated_at strictly after lastApplied
			time.Sleep(10 * time.Millisecond)
			Expect(store.SetRawYAML("ports:\n  http: 5353\n")).Should(Succeed())

			dirty, _, updatedAt, err = store.Status()
			Expect(err).Should(Succeed())
			Expect(dirty).Should(BeTrue())
			Expect(updatedAt.After(lastApplied)).Should(BeTrue())
		})
	})
})
