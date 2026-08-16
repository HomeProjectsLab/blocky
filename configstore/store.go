// Package configstore persists blocky's configuration as a raw YAML blob in a
// self-created SQLite file (config.db). It is the single source of truth for
// the fork's config: the CLI supervisor loads from it and the HTTP API edits it.
package configstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/log"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	dbFileName    = "config.db"
	busyTimeoutMs = 5000
	dirPermission = 0o750
)

// seedYAMLTemplate is the starter config written on first launch. %s is the
// querylog.db path inside the absolute db directory (YAML-escaped by seedYAML).
// The default group resolves recursively from the root servers; the quad9
// upstreams are its fallback tier.
const seedYAMLTemplate = `ports:
  http: 4000
upstreams:
  groups:
    default:
      - 9.9.9.9
      - 149.112.112.112
  groupConfig:
    default:
      strategy: recursive
queryLog:
  type: sqlite
  target: %s
`

// plainYAMLPath matches paths safe as a plain (unquoted) YAML scalar.
var plainYAMLPath = regexp.MustCompile(`^[0-9A-Za-z._~/-]+$`)

// seedYAML renders the starter config for absDir. The querylog target stays
// plain for ordinary paths (so IsFresh keeps matching blobs seeded before this
// existed) and is double-quoted otherwise, so a directory name containing
// ': ', ' #', a tab or a newline can't break the seed YAML and brick Open.
func seedYAML(absDir string) string {
	target := absDir + "/querylog.db"
	if !plainYAMLPath.MatchString(target) {
		target = strconv.Quote(target) // Go escapes are a subset of YAML double-quote escapes
	}

	return fmt.Sprintf(seedYAMLTemplate, target)
}

// configBlob is the single-row table holding the raw YAML config.
type configBlob struct {
	ID        int    `gorm:"primaryKey;check:id = 1"`
	YAML      string `gorm:"column:yaml;not null"`
	UpdatedAt time.Time
}

func (configBlob) TableName() string { return "config_blob" }

// Store is a SQLite-backed configuration store.
type Store struct {
	// db is an atomic handle: RestoreDB.reopen() swaps it while config readers
	// (blob/LoadConfig/list accessors) run lock-free — the swap must be atomic
	// or those reads race the pointer. Access only via conn().
	db     atomic.Pointer[gorm.DB]
	absDir string

	mu          sync.Mutex
	lastApplied time.Time

	// sessionSecret caches the HMAC key so the auth hot path (SessionSecret,
	// called per authenticated request) is a lock-free atomic read and never
	// holds s.mu across a config.db read. Without it, an IO stall on the
	// 1-connection config.db pool pins s.mu and freezes every auth request.
	// Written under s.mu (SessionSecret/RotateSessionSecret); cleared on
	// RestoreDB, whose swapped-in file may carry a different secret.
	sessionSecret atomic.Pointer[[]byte]

	// lastMutated is the unix-nano time of the last successful write to an
	// overlay table (see overlayTables). Atomic, not s.mu: the gorm hook fires
	// inside writers that already hold s.mu.
	lastMutated atomic.Int64

	applyCh chan struct{}
}

// Open creates dir if needed, opens (or creates) config.db inside it,
// migrates the schema and seeds the starter config if the table is empty.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, dirPermission); err != nil {
		return nil, fmt.Errorf("can't create config directory: %w", err)
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("can't resolve config directory: %w", err)
	}

	db, err := openGorm(absDir)
	if err != nil {
		return nil, err
	}

	if err := migrateSchema(db); err != nil {
		return nil, err
	}

	if err := seedIfEmpty(db, absDir); err != nil {
		return nil, err
	}

	if err := seedBlockingCategories(db); err != nil {
		return nil, err
	}

	s := &Store{
		absDir:  absDir,
		applyCh: make(chan struct{}, 1),
	}

	if err := s.hookDirtyTracking(db); err != nil {
		return nil, err
	}

	s.db.Store(db)

	log.PrefixedLog("configstore").WithField("path", s.DBPath()).Info("config store opened (schema migrated, seed ensured)")

	return s, nil
}

// conn returns the current database handle. It is swapped atomically by reopen()
// during an import, so every reader must fetch it through here.
func (s *Store) conn() *gorm.DB { return s.db.Load() }

// openGorm opens (creating if absent) config.db inside absDir and pins the pool
// to one connection (single local file, serialized like the querylog writer).
func openGorm(absDir string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open(buildDSN(filepath.Join(absDir, dbFileName))), &gorm.Config{
		Logger: logger.New(
			log.Log(),
			logger.Config{
				SlowThreshold:             time.Minute,
				LogLevel:                  logger.Warn,
				IgnoreRecordNotFoundError: false,
				Colorful:                  false,
			}),
	})
	if err != nil {
		return nil, fmt.Errorf("can't open config database: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("can't access config database connection pool: %w", err)
	}

	sqlDB.SetMaxOpenConns(1)

	return db, nil
}

func migrateSchema(db *gorm.DB) error {
	if err := db.AutoMigrate(&configBlob{}, &authSettings{}, &UpstreamGroup{}, &UpstreamEntry{},
		&BlockingCategory{}, &BlockingClientSegment{}, &AllowlistEntry{}, &DenylistEntry{}, &AdlistEntry{},
		&BlockingGroup{}, &BlockingGroupCategory{}, &BlockingGroupMember{}); err != nil {
		return fmt.Errorf("can't migrate config database schema: %w", err)
	}

	return nil
}

// overlayTables are the tables LoadConfig merges over the YAML blob. A write to
// any of them makes the running config stale without touching config_blob's
// updated_at, so Status() tracks them via lastMutated.
var overlayTables = map[string]bool{
	"upstream_group": true, "upstream_entry": true,
	"blocking_category": true, "blocking_client_segment": true,
	"allowlist_entry": true, "denylist_entry": true, "adlist_entry": true,
	"blocking_group": true, "blocking_group_category": true, "blocking_group_member": true,
}

// hookDirtyTracking bumps lastMutated after every successful overlay-table
// write on db, so Status() reports dirty for edits (category toggles,
// allow/deny/adlist/group changes) the blob's updated_at can't see.
func (s *Store) hookDirtyTracking(db *gorm.DB) error {
	touch := func(tx *gorm.DB) {
		if tx.Error == nil && overlayTables[tx.Statement.Table] {
			s.lastMutated.Store(time.Now().UnixNano())
		}
	}

	if err := db.Callback().Create().After("gorm:create").Register("configstore:dirty", touch); err != nil {
		return fmt.Errorf("can't register dirty-tracking hook: %w", err)
	}

	if err := db.Callback().Update().After("gorm:update").Register("configstore:dirty", touch); err != nil {
		return fmt.Errorf("can't register dirty-tracking hook: %w", err)
	}

	if err := db.Callback().Delete().After("gorm:delete").Register("configstore:dirty", touch); err != nil {
		return fmt.Errorf("can't register dirty-tracking hook: %w", err)
	}

	return nil
}

// DBPath returns the filesystem path of the underlying config database file.
func (s *Store) DBPath() string {
	return filepath.Join(s.absDir, dbFileName)
}

// IsFresh reports whether the stored blob still equals the untouched starter
// config seeded on first Open (i.e. nobody has ever changed the config).
func (s *Store) IsFresh() (bool, error) {
	raw, err := s.RawYAML()
	if err != nil {
		return false, err
	}

	return raw == seedYAML(s.absDir), nil
}

// buildDSN mirrors querylog's DSN: URI mode with percent-encoded path, WAL and busy_timeout.
func buildDSN(path string) string {
	encodedPath := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)

	return fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)", encodedPath, busyTimeoutMs)
}

func seedIfEmpty(db *gorm.DB, absDir string) error {
	var count int64
	if err := db.Model(&configBlob{}).Count(&count).Error; err != nil {
		return fmt.Errorf("can't read config blob: %w", err)
	}

	if count > 0 {
		return nil
	}

	seed := seedYAML(absDir)

	if _, err := config.LoadFromYAML([]byte(seed)); err != nil {
		return fmt.Errorf("seed config is invalid: %w", err)
	}

	if err := db.Create(&configBlob{ID: 1, YAML: seed}).Error; err != nil {
		return fmt.Errorf("can't seed config blob: %w", err)
	}

	return nil
}

func (s *Store) blob() (*configBlob, error) {
	var b configBlob
	if err := s.conn().First(&b, 1).Error; err != nil {
		return nil, fmt.Errorf("can't read config blob: %w", err)
	}

	return &b, nil
}

// LoadConfig reads the stored YAML and parses it through the full
// validation pipeline (defaults, strict unmarshal, migration, validation).
// If any upstream_group rows exist, they replace the blob's upstream groups
// entirely (see UpstreamGroup); if any blocking_category rows exist (seeded on
// Open) and the query log is sqlite, the blocking tables replace the blob's
// blocking lists the same way. The merged config is re-validated.
func (s *Store) LoadConfig() (*config.Config, error) {
	b, err := s.blob()
	if err != nil {
		return nil, err
	}

	cfg, err := config.LoadFromYAML([]byte(b.YAML))
	if err != nil {
		return nil, err
	}

	groups, entries, err := s.ListUpstreamGroups()
	if err != nil {
		return nil, err
	}

	overlaid := false
	blockingOverlaid := false

	if len(groups) > 0 {
		if err := overlayUpstreams(cfg, groups, entries); err != nil {
			return nil, err
		}

		overlaid = true
	}

	// blocking tables govern blocking only in sqlite query-log mode: the
	// category sources stream from that database (see lists.BlocklistProvider).
	if cfg.QueryLog.Type == config.QueryLogTypeSqlite {
		bl, err := s.loadBlockingRows()
		if err != nil {
			return nil, err
		}

		if bl.active() {
			overlayBlocking(cfg, bl)

			overlaid = true
			blockingOverlaid = true

			enabledCats, enabledAdlists := 0, 0
			for _, c := range bl.cats {
				if c.Enabled {
					enabledCats++
				}
			}
			for _, a := range bl.adlists {
				if a.Enabled {
					enabledAdlists++
				}
			}

			log.PrefixedLog("configstore").
				WithField("categories_enabled", enabledCats).
				WithField("categories_total", len(bl.cats)).
				WithField("adlists_enabled", enabledAdlists).
				WithField("client_segments", len(bl.segs)).
				Debug("blocking lists reloaded from store")
		}
	}

	// Debug, not Info: LoadConfig also backs GetPrivacy and runs on request
	// paths — at Info this line spammed the console once per privacy read.
	log.PrefixedLog("configstore").
		WithField("upstreams_overlaid", len(groups) > 0).
		WithField("blocking_overlaid", blockingOverlaid).
		Debug("config loaded")

	if !overlaid {
		return cfg, nil
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// RawYAML returns the stored YAML blob as-is.
func (s *Store) RawYAML() (string, error) {
	b, err := s.blob()
	if err != nil {
		return "", err
	}

	return b.YAML, nil
}

// SetRawYAML validates data through the full config pipeline and persists it
// only on success. The stored blob is untouched when validation fails. It takes
// s.mu so a raw-editor write serializes against the section writers
// (SetPrivacy/SetLocalDNSZone) the same way those already serialize against each
// other — otherwise a section writer built from a pre-save snapshot silently
// reverts this save.
func (s *Store) SetRawYAML(data string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.setRawYAML(data)
}

// setRawYAML is the unlocked body of SetRawYAML. Callers that already hold s.mu
// (SetPrivacy, SetLocalDNSZone) call this directly to avoid self-deadlock.
func (s *Store) setRawYAML(data string) error {
	if err := s.ValidateRaw(data); err != nil {
		return err
	}

	res := s.conn().Model(&configBlob{}).Where("id = 1").
		Updates(map[string]any{"yaml": data, "updated_at": time.Now()})
	if res.Error != nil {
		return fmt.Errorf("can't persist config blob: %w", res.Error)
	}

	if res.RowsAffected == 0 {
		return errors.New("config blob row missing")
	}

	log.PrefixedLog("configstore").WithField("bytes", len(data)).Info("raw config validated and persisted")

	return nil
}

// ValidateRaw runs the full config validation pipeline without persisting.
func (s *Store) ValidateRaw(data string) error {
	_, err := config.LoadFromYAML([]byte(data))

	return err
}

// RequestApply signals the supervisor to rebuild the server. Non-blocking:
// a pending signal is not duplicated.
func (s *Store) RequestApply() {
	select {
	case s.applyCh <- struct{}{}:
		log.PrefixedLog("configstore").Info("config apply requested")
	default:
	}
}

// ApplyRequested returns the channel the supervisor listens on for apply signals.
func (s *Store) ApplyRequested() <-chan struct{} {
	return s.applyCh
}

// MarkApplied records now as the time the current config was applied (in memory only).
func (s *Store) MarkApplied() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastApplied = time.Now()
}

// Status reports whether the stored config has changed since the last apply.
// dirty is true when the blob's updated_at OR the last overlay-table write
// (lastMutated) is after lastApplied, or when nothing has been applied yet
// (lastApplied zero).
func (s *Store) Status() (dirty bool, lastApplied, updatedAt time.Time, err error) {
	b, err := s.blob()
	if err != nil {
		return false, time.Time{}, time.Time{}, err
	}

	s.mu.Lock()
	lastApplied = s.lastApplied
	s.mu.Unlock()

	updatedAt = b.UpdatedAt
	if m := time.Unix(0, s.lastMutated.Load()); m.After(updatedAt) {
		updatedAt = m
	}

	dirty = lastApplied.IsZero() || updatedAt.After(lastApplied)

	return dirty, lastApplied, updatedAt, nil
}

// SnapshotTo writes a consistent, standalone copy of config.db to path.
// VACUUM INTO is WAL-safe and checkpoints on its own; the whole file is copied
// (not RawYAML) because the overlay tables carry config LoadConfig merges in.
func (s *Store) SnapshotTo(path string) error {
	// s.mu serializes against RestoreDB (handle close + file rename mid-copy)
	// and a concurrent export's TRUNCATE checkpoint rewriting the main file,
	// either of which would tear the byte-copy below.
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.conn().Exec("VACUUM INTO ?", path).Error; err == nil {
		return nil
	}

	// ponytail: fallback for a driver that rejects VACUUM INTO (glebarez/modernc
	// quirk). Checkpoint the WAL into the main file, then byte-copy. Upgrade path:
	// drop this once the driver is confirmed to support VACUUM INTO everywhere.
	if err := s.conn().Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error; err != nil {
		return fmt.Errorf("can't checkpoint config database: %w", err)
	}

	return copyFile(s.DBPath(), path)
}

// RestoreDB atomically swaps the live config.db for the file at newPath, which
// the caller MUST have validated already. It holds s.mu (serializing against the
// section/overlay writers, not against lock-free config reads — those ride the
// atomic conn() handle) while it closes the handle, backs the current file up to
// .bak, moves the new file in and reopens. Any failure rolls back from the .bak.
func (s *Store) RestoreDB(newPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rlog := log.PrefixedLog("configstore")
	rlog.WithField("from", newPath).Info("restoring config database")

	dbPath := s.DBPath()
	bakPath := dbPath + ".bak"

	sqlDB, err := s.conn().DB()
	if err != nil {
		return err
	}

	if err := sqlDB.Close(); err != nil {
		return fmt.Errorf("can't close config database: %w", err)
	}

	// the WAL is checkpointed into the main file on close; drop the sidecars so
	// a stale one is never applied to the file we move in under the same name.
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")

	restore := func(cause error) error {
		rlog.WithField("error", cause.Error()).Warn("config database restore failed, rolling back to previous database")
		_ = os.Rename(bakPath, dbPath)
		if reErr := s.reopen(); reErr != nil {
			return fmt.Errorf("%w; and can't reopen original: %v", cause, reErr)
		}

		return cause
	}

	if err := os.Rename(dbPath, bakPath); err != nil {
		rlog.WithField("error", err.Error()).Warn("config database restore failed to back up current database, rolling back")
		_ = s.reopen()

		return fmt.Errorf("can't back up config database: %w", err)
	}

	if err := os.Rename(newPath, dbPath); err != nil {
		return restore(fmt.Errorf("can't move restored database in: %w", err))
	}

	if err := s.reopen(); err != nil {
		_ = os.Remove(dbPath)

		return restore(fmt.Errorf("can't open restored database: %w", err))
	}

	_ = os.Remove(bakPath)

	// A whole-file swap bypasses the overlay-table dirty hooks and may carry an
	// OLDER updated_at than lastApplied: mark dirty explicitly so Status()
	// reports the restored config as not applied yet.
	s.lastMutated.Store(time.Now().UnixNano())
	s.lastApplied = time.Time{}

	// the swapped-in file may carry a different session secret; drop the cache
	// so the next SessionSecret re-reads it from the restored database.
	s.sessionSecret.Store(nil)

	rlog.Info("config database restored")

	return nil
}

// reopen rebuilds s.db from absDir (config.db must already exist) and migrates.
func (s *Store) reopen() error {
	db, err := openGorm(s.absDir)
	if err != nil {
		return err
	}

	if err := migrateSchema(db); err != nil {
		return err
	}

	if err := s.hookDirtyTracking(db); err != nil {
		return err
	}

	s.db.Store(db)

	return nil
}

// VerifyConfigDB opens the SQLite file at path strictly read-only — no
// AutoMigrate, no seeding — and checks it actually is a config database
// (exactly one config_blob row). Guard rail before validateConfigDB/RestoreDB:
// Open() happily migrates+seeds ANY SQLite file (e.g. a querylog.db) into
// something that "validates", and the restore would then wipe the live config.
func VerifyConfigDB(path string) error {
	encodedPath := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(%d)", encodedPath, busyTimeoutMs)

	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return fmt.Errorf("can't open uploaded database: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("can't access uploaded database: %w", err)
	}
	defer sqlDB.Close()

	var count int64
	if err := db.Table("config_blob").Count(&count).Error; err != nil {
		return fmt.Errorf("not a config database: %w", err)
	}

	if count != 1 {
		return fmt.Errorf("not a config database: config_blob has %d rows, want 1", count)
	}

	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()

		return err
	}

	return out.Close()
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	sqlDB, err := s.conn().DB()
	if err != nil {
		return err
	}

	return sqlDB.Close()
}
