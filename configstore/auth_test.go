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

	first := store.SessionSecret()
	if len(first) != sessionSecretLen {
		t.Fatalf("secret len = %d, want %d", len(first), sessionSecretLen)
	}

	if !bytes.Equal(first, store.SessionSecret()) {
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

	if !bytes.Equal(first, reopened.SessionSecret()) {
		t.Fatal("secret did not persist across reopen")
	}
}
