// Package secrets stores all credentials so the model never sees a key (invariant
// I3, docs/SECRETS.md). A SecretStore gets/sets/deletes secrets by name; values
// are held only transiently and are never written to disk in plaintext, never
// logged, and never placed in a prompt. Backends, auto-detected: the OS keychain,
// an encrypted-file vault (for headless hosts), the environment (read-only), and
// an external command hook. Stdlib only (invariant I6).
package secrets

import "errors"

// ErrNotFound is returned by Get when a named secret does not exist.
var ErrNotFound = errors.New("secret not found")

// ErrReadOnly is returned by Set/Delete on a read-only backend (env).
var ErrReadOnly = errors.New("secret store is read-only")

// SecretStore is the credential boundary. Implementations must never log or
// otherwise expose a secret value; error messages reference the secret name only.
type SecretStore interface {
	Get(name string) (string, error)
	Set(name, value string) error
	Delete(name string) error
	Name() string // backend name (for logging which backend, never the secret)
}

// Detect picks the best zero-config backend for this host: the OS keychain when
// its CLI is present, otherwise the read-only environment store. The encrypted
// file vault and external hook need explicit configuration (a key / a command),
// so callers construct those directly.
func Detect() SecretStore {
	if keychainAvailable() {
		return KeychainStore{}
	}
	return EnvStore{}
}
