package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOrCreateSalt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secrets.salt")

	s1, err := ReadOrCreateSalt(p, true)
	if err != nil || len(s1) != 16 {
		t.Fatalf("create: err=%v len=%d", err, len(s1))
	}
	if info, _ := os.Stat(p); info == nil || info.Mode().Perm() != 0o600 {
		t.Errorf("salt file perms = %v, want 0600", info.Mode().Perm())
	}
	// A second read returns the same salt (stable across runs).
	s2, err := ReadOrCreateSalt(p, false)
	if err != nil || !bytes.Equal(s1, s2) {
		t.Fatalf("read mismatch: %x vs %x (%v)", s1, s2, err)
	}
	// Missing salt with create=false is an error (boot must not mint a new one).
	if _, err := ReadOrCreateSalt(filepath.Join(t.TempDir(), "absent.salt"), false); err == nil {
		t.Error("missing salt with create=false must error")
	}
}

// A passphrase + salt derives a stable key that seals and opens the vault, and a
// different passphrase cannot decrypt it.
func TestPassphraseVaultSeal(t *testing.T) {
	dir := t.TempDir()
	salt, err := ReadOrCreateSalt(filepath.Join(dir, "secrets.salt"), true)
	if err != nil {
		t.Fatal(err)
	}
	key := MasterKeyFromPassphrase("correct horse", salt, 0)
	v, err := OpenFileVault(filepath.Join(dir, "secrets.vault"), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("k", "secret-value"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "secrets.vault"))
	if bytes.Contains(raw, []byte("secret-value")) {
		t.Fatal("plaintext in passphrase vault — I3 violation")
	}
	// Re-derive from the same passphrase+salt and read it back.
	v2, _ := OpenFileVault(filepath.Join(dir, "secrets.vault"), MasterKeyFromPassphrase("correct horse", salt, 0))
	if got, err := v2.Get("k"); err != nil || got != "secret-value" {
		t.Fatalf("round-trip = %q, %v", got, err)
	}
	// A different passphrase derives a different key that cannot decrypt.
	vBad, _ := OpenFileVault(filepath.Join(dir, "secrets.vault"), MasterKeyFromPassphrase("wrong", salt, 0))
	if _, err := vBad.Get("k"); err == nil {
		t.Error("wrong passphrase must not decrypt")
	}
}
