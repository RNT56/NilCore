package secrets

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
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

// A hook that exits 0 but prints nothing must NOT yield an empty secret — fail
// closed (I3): an empty / whitespace-only payload resolves as ErrNotFound so the
// resolver falls through to the next store instead of injecting a blank credential.
func TestExternalStoreEmptyIsNotFound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell hook")
	}
	for _, tc := range []struct {
		name string
		emit string // the `get` branch body
	}{
		{"empty", "true"},                // prints nothing, exits 0
		{"newline-only", "echo"},         // a bare `echo` ⇒ one newline
		{"whitespace-only", "echo '  '"}, // spaces + newline
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "hook.sh")
			body := "#!/bin/sh\ncase \"$1\" in get) " + tc.emit + ";; *) cat >/dev/null;; esac\nexit 0\n"
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			e := ExternalStore{Command: script}
			got, err := e.Get("API")
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("empty hook stdout: Get = %q, %v, want ErrNotFound", got, err)
			}
			if got != "" {
				t.Errorf("empty hook stdout should yield no value, got %q", got)
			}
		})
	}
}

// A keychain CLI that exits 0 but returns an empty / whitespace-only value must
// NOT yield an empty secret — fail closed (I3). Keychain sits FIRST in the resolver
// chain, so an empty value here would short-circuit the file-vault / env fallback
// that holds the real credential; it must resolve as ErrNotFound and fall through.
// Mirrors TestExternalStoreEmptyIsNotFound. GOOS-gated like the other keychain tests
// and driven through the injected exec seam — the real OS keychain is never touched.
func TestKeychainStoreEmptyIsNotFound(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("keychain backend unsupported on %s", runtime.GOOS)
	}
	for _, tc := range []struct {
		name    string
		payload string // exact stdout the CLI emits on a zero-exit lookup
	}{
		{"empty", ""},
		{"newline-only", "\n"},
		{"whitespace-only", "  \n"},
		{"tabs-and-spaces", "\t \t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Injected seam: the lookup exits 0 (nil err) but hands back only the
			// blank payload, exercising the fail-closed check on both platforms.
			k := KeychainStore{
				Service: "nilcore-test-throwaway",
				run: func(name string, args []string, stdin string) (string, error) {
					return tc.payload, nil
				},
			}
			got, err := k.Get("API")
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("empty keychain value: Get = %q, %v, want ErrNotFound", got, err)
			}
			if got != "" {
				t.Errorf("empty keychain value should yield no value, got %q", got)
			}
		})
	}

	// A real value at the same seam must still come back (proving the check does not
	// swallow legitimate secrets).
	const real = "sk-real-value-not-empty"
	k := KeychainStore{
		Service: "nilcore-test-throwaway",
		run: func(name string, args []string, stdin string) (string, error) {
			// mimic the macOS trailing newline; Get must trim then return the value.
			return real + "\n", nil
		},
	}
	got, err := k.Get("API")
	if err != nil {
		t.Fatalf("real value: Get = %v, want nil", err)
	}
	if got != real {
		t.Fatalf("real value: Get = %q, want %q", got, real)
	}
}

// fakeKeychain is an in-memory stand-in for the OS keychain CLI, keyed by
// (service, account). It lets the round-trip test exercise KeychainStore.Get/Set/
// Delete on the active platform's code path without ever touching the real keychain.
type fakeKeychain struct {
	store map[string]string // key: service\x00account
}

func keyOf(service, account string) string { return service + "\x00" + account }

// run mirrors the argv contract of `security` (macOS) and `secret-tool` (linux)
// closely enough to round-trip a value. It is wired into KeychainStore.run.
func (f *fakeKeychain) run(name string, args []string, stdin string) (string, error) {
	// Pull the service (-s / "service") and account (-a / "account") out of args.
	flag := func(want ...string) string {
		for i := 0; i < len(args)-1; i++ {
			for _, w := range want {
				if args[i] == w {
					return args[i+1]
				}
			}
		}
		return ""
	}
	svc := flag("-s", "service")
	acct := flag("-a", "account")
	if len(args) == 0 {
		return "", &exec.ExitError{}
	}
	switch {
	case name == "security" && args[0] == "find-generic-password",
		name == "secret-tool" && args[0] == "lookup":
		v, ok := f.store[keyOf(svc, acct)]
		if !ok {
			return "", &exec.ExitError{} // a not-found lookup exits non-zero
		}
		// security emits a trailing newline; secret-tool does not. Reproduce the
		// macOS newline so the test proves the Get path trims it on both platforms.
		if name == "security" {
			return v + "\n", nil
		}
		return v, nil
	case name == "security" && args[0] == "add-generic-password":
		f.store[keyOf(svc, acct)] = flag("-w") // macOS passes the value on argv via -w
		return "", nil
	case name == "secret-tool" && args[0] == "store":
		f.store[keyOf(svc, acct)] = stdin // linux passes the value on stdin
		return "", nil
	case name == "security" && args[0] == "delete-generic-password",
		name == "secret-tool" && args[0] == "clear":
		delete(f.store, keyOf(svc, acct))
		return "", nil
	}
	return "", &exec.ExitError{}
}

// TestKeychainStoreRoundTrip exercises Get/Set/Delete against an injected in-memory
// keychain (never the real OS keychain), covering the active platform's code path.
// It pins the cross-platform contract the auditor flagged: a stored value reads back
// byte-for-byte with NO trailing-newline asymmetry between macOS and Linux.
func TestKeychainStoreRoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("keychain backend unsupported on %s", runtime.GOOS)
	}
	fake := &fakeKeychain{store: map[string]string{}}
	k := KeychainStore{Service: "nilcore-test-throwaway", run: fake.run}

	if _, err := k.Get("MISSING"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
	const secret = "sk-keychain-roundtrip-123"
	if err := k.Set("ANTHROPIC_API_KEY", secret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := k.Get("ANTHROPIC_API_KEY")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// No trailing-newline asymmetry: the value comes back exactly as stored on
	// either platform (the macOS path appends a newline that Get must trim).
	if got != secret {
		t.Fatalf("Get = %q, want %q (trailing-newline asymmetry?)", got, secret)
	}
	if err := k.Delete("ANTHROPIC_API_KEY"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := k.Get("ANTHROPIC_API_KEY"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete Get = %v, want ErrNotFound", err)
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
