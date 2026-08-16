package configstore

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/0xERR0R/blocky/log"
	"golang.org/x/crypto/argon2"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// authSettings holds the web-UI login secrets. It lives in its own table (never
// the YAML blob) so it survives a raw-config save: the Settings textarea
// round-trips the whole blob, which would otherwise clobber these on every edit.
// Single fixed row (id=1). TotpSecret is reserved for a future 2FA and unused.
type authSettings struct {
	ID            int `gorm:"primaryKey;check:id = 1"`
	PasswordHash  string
	SessionSecret []byte
	TotpSecret    []byte
}

func (authSettings) TableName() string { return "auth_settings" }

const sessionSecretLen = 32

// argon2id parameters (OWASP-ish defaults; tune memory down for tiny boxes).
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// authRow reads the singleton row, returning a fresh id=1 zero-value when the
// row does not exist yet. Callers hold s.mu.
func (s *Store) authRow() (*authSettings, error) {
	var a authSettings

	err := s.conn().First(&a, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &authSettings{ID: 1}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("can't read auth settings: %w", err)
	}

	return &a, nil
}

// saveAuthRow upserts the singleton row. Callers hold s.mu and pass a row loaded
// via authRow so untouched fields (e.g. SessionSecret) are preserved.
func (s *Store) saveAuthRow(a *authSettings) error {
	a.ID = 1
	if err := s.conn().Clauses(clause.OnConflict{UpdateAll: true}).Create(a).Error; err != nil {
		return fmt.Errorf("can't persist auth settings: %w", err)
	}

	return nil
}

// IsAuthConfigured reports whether a UI password has been set. On a read error it
// fails closed (reports false), which makes the gate demand setup rather than
// silently opening.
func (s *Store) IsAuthConfigured() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, err := s.authRow()
	if err != nil {
		log.Log().Error("auth: ", err)

		return false
	}

	return a.PasswordHash != ""
}

// SetPassword hashes pw with argon2id and stores it.
func (s *Store) SetPassword(pw string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, err := s.authRow()
	if err != nil {
		return err
	}

	hash, err := hashPassword(pw)
	if err != nil {
		return err
	}

	a.PasswordHash = hash

	return s.saveAuthRow(a)
}

// VerifyPassword constant-time compares pw against the stored argon2id hash.
func (s *Store) VerifyPassword(pw string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, err := s.authRow()
	if err != nil || a.PasswordHash == "" {
		return false
	}

	return verifyPassword(a.PasswordHash, pw)
}

// SessionSecret returns the HMAC key for signed session cookies, generating and
// persisting a fresh 32-byte secret on first read.
func (s *Store) SessionSecret() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, err := s.authRow()
	if err != nil {
		log.Log().Error("auth: ", err)

		return nil
	}

	if len(a.SessionSecret) == sessionSecretLen {
		return a.SessionSecret
	}

	secret := make([]byte, sessionSecretLen)
	if _, err := rand.Read(secret); err != nil {
		log.Log().Error("auth: can't generate session secret: ", err)

		return nil
	}

	a.SessionSecret = secret
	if err := s.saveAuthRow(a); err != nil {
		// return the generated secret anyway: cookies just won't survive a restart
		log.Log().Error("auth: ", err)
	}

	return secret
}

// RotateSessionSecret replaces the session secret, invalidating every live cookie.
func (s *Store) RotateSessionSecret() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, err := s.authRow()
	if err != nil {
		return err
	}

	secret := make([]byte, sessionSecretLen)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("can't generate session secret: %w", err)
	}

	a.SessionSecret = secret

	return s.saveAuthRow(a)
}

// hashPassword returns a PHC-format argon2id string ($argon2id$v=..$m=..,t=..,p=..$salt$hash).
func hashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("can't generate salt: %w", err)
	}

	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// verifyPassword recomputes the hash of pw using the encoded parameters and
// constant-time compares. Any parse failure is a non-match.
func verifyPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$") // ["", "argon2id", "v=..", "m=..,t=..,p=..", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}

	var (
		memory  uint32
		time    uint32
		threads uint8
	)

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}

	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}

	got := argon2.IDKey([]byte(pw), salt, time, memory, threads, uint32(len(want)))

	return subtle.ConstantTimeCompare(got, want) == 1
}
