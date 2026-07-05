package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// FileStore is an encrypted-file vault for headless hosts: a 0600 JSON file of
// name → AES-256-GCM ciphertext, sealed under a 32-byte master key. Plaintext
// never touches disk. The master key comes from a key file or a passphrase (see
// MasterKeyFromFile / MasterKeyFromPassphrase) — managed by the caller, never
// stored in the vault.
type FileStore struct {
	path string
	key  []byte
	mu   sync.Mutex
}

type vaultFile struct {
	Version int               `json:"version"`
	Secrets map[string]string `json:"secrets"`
}

// OpenFileVault returns a vault at path sealed with the given 32-byte key.
func OpenFileVault(path string, key []byte) (*FileStore, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(key))
	}
	return &FileStore{path: path, key: key}, nil
}

// Name identifies the backend.
func (f *FileStore) Name() string { return "file" }

// Get decrypts and returns the named secret.
func (f *FileStore) Get(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.load()
	if err != nil {
		return "", err
	}
	enc, ok := v.Secrets[name]
	if !ok {
		return "", fmt.Errorf("secret %q: %w", name, ErrNotFound)
	}
	return f.open(enc)
}

// Set seals value under the master key and writes it to the vault.
func (f *FileStore) Set(name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.load()
	if err != nil {
		return err
	}
	enc, err := f.seal(value)
	if err != nil {
		return err
	}
	v.Secrets[name] = enc
	return f.save(v)
}

// Delete removes the named secret from the vault.
func (f *FileStore) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.load()
	if err != nil {
		return err
	}
	delete(v.Secrets, name)
	return f.save(v)
}

func (f *FileStore) load() (vaultFile, error) {
	b, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return vaultFile{Version: 1, Secrets: map[string]string{}}, nil
	}
	if err != nil {
		return vaultFile{}, fmt.Errorf("read vault: %w", err)
	}
	var v vaultFile
	if err := json.Unmarshal(b, &v); err != nil {
		return vaultFile{}, fmt.Errorf("vault corrupt: %w", err)
	}
	if v.Secrets == nil {
		v.Secrets = map[string]string{}
	}
	return v, nil
}

func (f *FileStore) save(v vaultFile) error {
	v.Version = 1
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return fmt.Errorf("vault dir: %w", err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write: a crash mid-write must not truncate/corrupt the vault and lose
	// EVERY secret. Write a sibling temp file, fsync it, then rename over the vault
	// (rename is atomic on the same filesystem). 0o600 throughout.
	return writeFileAtomic(f.path, b, 0o600)
}

// writeFileAtomic writes data to a temp file in the target's directory, fsyncs it, and
// renames it over path so a reader never sees a partial file and a crash never corrupts
// the existing one. The temp file is removed on any error before the rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".vault-*.tmp")
	if err != nil {
		return fmt.Errorf("vault temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

func (f *FileStore) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(f.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (f *FileStore) seal(plain string) (string, error) {
	gcm, err := f.gcm()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (f *FileStore) open(enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("decode vault entry: %w", err)
	}
	gcm, err := f.gcm()
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("vault entry too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	return string(plain), nil
}

// MasterKeyFromFile reads a 32-byte master key from a 0600 key file, provisioning
// a fresh random one if the file does not exist (headless-VPS default).
//
// SECURITY (I3): the key sits beside the vault, so its 0600 mode is the only thing
// guarding every stored secret. Permissions are checked and tightened BEFORE the
// bytes are read — a loose key file (backup restore, rsync without -p, a bad umask)
// is chmod'd back to 0600 and RE-VERIFIED before we read it, so we never read a key
// while it is still world/group-readable and then "self-heal" too late (the old
// TOCTOU: another process could read the plaintext key in the window between read
// and chmod). If the file remains group/other-readable after the tighten attempt
// (e.g. we are not the owner, or an OS that ignores chmod bits), we refuse rather
// than silently loading a persistently-exposed key.
func MasterKeyFromFile(keyPath string) ([]byte, error) {
	fi, statErr := os.Stat(keyPath)
	if errors.Is(statErr, os.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(keyPath, key, 0o600); err != nil {
			return nil, fmt.Errorf("write master key: %w", err)
		}
		return key, nil
	}
	if statErr != nil {
		return nil, fmt.Errorf("stat master key: %w", statErr)
	}

	// Tighten-then-verify BEFORE reading. A regular file loosened past 0600 is
	// chmod'd back and re-stat'd; if it is still group/other-readable we refuse.
	if fi.Mode().IsRegular() && fi.Mode().Perm()&0o077 != 0 {
		if chErr := os.Chmod(keyPath, 0o600); chErr != nil {
			return nil, fmt.Errorf("master key file %s is group/other-readable and could not be tightened to 0600: %w", keyPath, chErr)
		}
		fi2, reErr := os.Stat(keyPath)
		if reErr != nil {
			return nil, fmt.Errorf("re-stat master key after tighten: %w", reErr)
		}
		if fi2.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("master key file %s remains group/other-readable (%04o) after tightening; refusing to load an exposed key", keyPath, fi2.Mode().Perm())
		}
	}

	b, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	if len(b) < 32 {
		return nil, fmt.Errorf("master key file %s shorter than 32 bytes", keyPath)
	}
	return b[:32], nil
}

// ReadOrCreateSalt returns the PBKDF2 salt stored at path. When create is set and
// the file is absent, it generates a fresh random 16-byte salt and persists it
// (0600). The salt is not secret — it only needs to be stable across runs so the
// same passphrase derives the same key — but a per-vault random salt defeats
// precomputation. With create false, a missing file is an error (boot must not
// silently mint a new salt that would derive a key unable to decrypt the vault).
func ReadOrCreateSalt(path string, create bool) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		return b, nil
	}
	if !create || !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, salt, 0o600); err != nil {
		return nil, fmt.Errorf("write salt: %w", err)
	}
	return salt, nil
}

// MasterKeyFromPassphrase derives a 32-byte key via PBKDF2-HMAC-SHA256 (stdlib).
// salt must be stable across runs (store it next to the vault; it is not secret).
func MasterKeyFromPassphrase(passphrase string, salt []byte, iterations int) []byte {
	if iterations <= 0 {
		iterations = 200_000
	}
	return pbkdf2SHA256([]byte(passphrase), salt, iterations, 32)
}

// pbkdf2SHA256 implements PBKDF2 (RFC 8018) over HMAC-SHA256 using only stdlib,
// so the zero-dependency invariant (I6) holds.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	hashLen := sha256.Size
	blocks := (keyLen + hashLen - 1) / hashLen
	dk := make([]byte, 0, blocks*hashLen)
	buf := make([]byte, 4)
	for block := 1; block <= blocks; block++ {
		prf := hmac.New(sha256.New, password)
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf, uint32(block))
		prf.Write(buf)
		u := prf.Sum(nil)
		t := make([]byte, hashLen)
		copy(t, u)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for x := range t {
				t[x] ^= u[x]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}
