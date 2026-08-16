package configstore

import (
	"bytes"
	"testing"
)

func TestPasswordRoundTrip(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	if store.IsAuthConfigured() {
		t.Fatal("fresh store should be unconfigured")
	}

	if err := store.SetPassword("correct horse battery staple"); err != nil {
		t.Fatalf("set password: %v", err)
	}

	if !store.IsAuthConfigured() {
		t.Fatal("store should be configured after SetPassword")
	}

	if !store.VerifyPassword("correct horse battery staple") {
		t.Fatal("correct password rejected")
	}

	if store.VerifyPassword("wrong") {
		t.Fatal("wrong password accepted")
	}
}

func TestSessionSecretPersists(t *testing.T) {
	dir := t.TempDir()

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	first, err := store.SessionSecret()
	if err != nil {
		t.Fatalf("session secret: %v", err)
	}

	if len(first) != sessionSecretLen {
		t.Fatalf("secret len = %d, want %d", len(first), sessionSecretLen)
	}

	second, err := store.SessionSecret()
	if err != nil {
		t.Fatalf("session secret re-read: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Fatal("secret changed between reads on the same store")
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	persisted, err := reopened.SessionSecret()
	if err != nil {
		t.Fatalf("session secret after reopen: %v", err)
	}

	if !bytes.Equal(first, persisted) {
		t.Fatal("secret did not persist across reopen")
	}
}

// Regression: a store read error must surface as an error, never as a nil
// secret — a nil key would make the session gate build an empty-key HMAC that
// an attacker can forge offline.
func TestSessionSecretFailsClosed(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Open now warms the cache, so a closed store would otherwise serve the cached
	// secret lock-free (the intended resilience). Clear it to exercise the path
	// this test guards: a cache-miss that hits the DB and gets a read error.
	store.sessionSecret.Store(nil)

	secret, err := store.SessionSecret()
	if err == nil {
		t.Fatal("SessionSecret on a closed store should error")
	}

	if secret != nil {
		t.Fatal("SessionSecret must not return a key alongside an error")
	}
}
