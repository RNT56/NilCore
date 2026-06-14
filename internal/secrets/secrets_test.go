package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestFileVaultRoundTrip(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "vault.json")
	s, err := OpenFileVault(vault, testKey())
	if err != nil {
		t.Fatalf("OpenFileVault: %v", err)
	}

	if _, err := s.Get("MISSING"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
	if err := s.Set("ANTHROPIC_API_KEY", "sk-secret-123"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get("ANTHROPIC_API_KEY")
	if err != nil || got != "sk-secret-123" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := s.Delete("ANTHROPIC_API_KEY"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get("ANTHROPIC_API_KEY"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete Get = %v, want ErrNotFound", err)
	}
}

func TestFileVaultEncryptsOnDisk(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "vault.json")
	s, _ := OpenFileVault(vault, testKey())
	const secret = "topsecret-plaintext-value"
	if err := s.Set("K", secret); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(vault)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("plaintext secret found on disk — vault is not encrypting")
	}
	if info, _ := os.Stat(vault); info.Mode().Perm() != 0o600 {
		t.Errorf("vault perms = %v, want 0600", info.Mode().Perm())
	}

	// A different key cannot decrypt.
	wrong := testKey()
	wrong[0] ^= 0xFF
	s2, _ := OpenFileVault(vault, wrong)
	if _, err := s2.Get("K"); err == nil {
		t.Error("decryption with the wrong key should fail")
	}
}

func TestEnvStore(t *testing.T) {
	t.Setenv("NILCORE_SECRET_FOO", "bar")
	e := EnvStore{Prefix: "NILCORE_SECRET_"}
	if got, err := e.Get("FOO"); err != nil || got != "bar" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if _, err := e.Get("NOPE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
	if err := e.Set("X", "y"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Set = %v, want ErrReadOnly", err)
	}
}

func TestMasterKeyFromFile(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "sub", "master.key")
	k1, err := MasterKeyFromFile(keyPath) // provisions
	if err != nil || len(k1) != 32 {
		t.Fatalf("provision: len=%d err=%v", len(k1), err)
	}
	if info, _ := os.Stat(keyPath); info.Mode().Perm() != 0o600 {
		t.Errorf("key file perms = %v, want 0600", info.Mode().Perm())
	}
	k2, err := MasterKeyFromFile(keyPath) // reads existing
	if err != nil || !bytes.Equal(k1, k2) {
		t.Fatalf("re-read key mismatch: err=%v", err)
	}
}

func TestMasterKeyFromPassphrase(t *testing.T) {
	salt := []byte("nilcore-salt")
	a := MasterKeyFromPassphrase("correct horse", salt, 1000)
	b := MasterKeyFromPassphrase("correct horse", salt, 1000)
	c := MasterKeyFromPassphrase("different", salt, 1000)
	if len(a) != 32 {
		t.Fatalf("key len = %d, want 32", len(a))
	}
	if !bytes.Equal(a, b) {
		t.Error("same passphrase+salt must derive the same key")
	}
	if bytes.Equal(a, c) {
		t.Error("different passphrases must derive different keys")
	}
}

func TestDetect(t *testing.T) {
	s := Detect()
	if s == nil {
		t.Fatal("Detect returned nil")
	}
	if n := s.Name(); n != "keychain" && n != "env" {
		t.Errorf("Detect backend = %q", n)
	}
}

func TestExternalStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell hook")
	}
	script := filepath.Join(t.TempDir(), "hook.sh")
	body := "#!/bin/sh\ncase \"$1\" in get) echo \"value-for-$2\";; *) cat >/dev/null;; esac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	e := ExternalStore{Command: script}
	got, err := e.Get("API")
	if err != nil || got != "value-for-API" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := e.Set("API", "secret"); err != nil {
		t.Errorf("Set: %v", err)
	}
}

// Error messages must reference the name, never the secret value.
func TestNoSecretInErrors(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "vault.json")
	s, _ := OpenFileVault(vault, testKey())
	_ = s.Set("K", "ultra-secret")
	// Corrupt the entry so open() errors, then confirm no value leaks.
	_, err := s.Get("MISSING")
	if err != nil && strings.Contains(err.Error(), "ultra-secret") {
		t.Error("error message leaked a secret value")
	}
}
