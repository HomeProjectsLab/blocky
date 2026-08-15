// Package configstore persists blocky's configuration as a raw YAML blob in a
// self-created SQLite file (config.db). It is the single source of truth for
// the fork's config: the CLI supervisor loads from it and the HTTP API edits it.
package configstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
// absolute db directory. The default group resolves recursively from the root
// servers; the quad9 upstreams are its fallback tier.
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
  target: %s/querylog.db
`

// configBlob is the single-row table holding the raw YAML config.
type configBlob struct {
	ID        int    `gorm:"primaryKey;check:id = 1"`
	YAML      string `gorm:"column:yaml;not null"`
	UpdatedAt time.Time
}

func (configBlob) TableName() string { return "config_blob" }

// Store is a SQLite-backed configuration store.
type Store struct {
	db     *gorm.DB
	absDir string

	mu          sync.Mutex
	lastApplied time.Time

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

	// single local file: serialize through one connection (see querylog writer)
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(&configBlob{}, &UpstreamGroup{}, &UpstreamEntry{},
		&BlockingCategory{}, &BlockingClientSegment{}, &AllowlistEntry{}, &DenylistEntry{}); err != nil {
		return nil, fmt.Errorf("can't migrate config database schema: %w", err)
	}

	if err := seedIfEmpty(db, absDir); err != nil {
		return nil, err
	}

	if err := seedBlockingCategories(db); err != nil {
		return nil, err
	}

	return &Store{
		db:      db,
		absDir:  absDir,
		applyCh: make(chan struct{}, 1),
	}, nil
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

	return raw == fmt.Sprintf(seedYAMLTemplate, s.absDir), nil
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

	seed := fmt.Sprintf(seedYAMLTemplate, absDir)

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
	if err := s.db.First(&b, 1).Error; err != nil {
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
		}
	}

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

	res := s.db.Model(&configBlob{}).Where("id = 1").
		Updates(map[string]any{"yaml": data, "updated_at": time.Now()})
	if res.Error != nil {
		return fmt.Errorf("can't persist config blob: %w", res.Error)
	}

	if res.RowsAffected == 0 {
		return errors.New("config blob row missing")
	}

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

// Status reports whether the stored blob has changed since the last apply.
// dirty is true when the blob's updated_at is after lastApplied, or when
// nothing has been applied yet (lastApplied zero).
func (s *Store) Status() (dirty bool, lastApplied, updatedAt time.Time, err error) {
	b, err := s.blob()
	if err != nil {
		return false, time.Time{}, time.Time{}, err
	}

	s.mu.Lock()
	lastApplied = s.lastApplied
	s.mu.Unlock()

	dirty = lastApplied.IsZero() || b.UpdatedAt.After(lastApplied)

	return dirty, lastApplied, b.UpdatedAt, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}

	return sqlDB.Close()
}
