package secrets

import (
	"fmt"
	"os"
)

// EnvStore reads secrets from environment variables (optionally under a prefix).
// It is read-only: secrets enter the environment from the host's own mechanism
// (systemd EnvironmentFile, shell export), never written by NilCore.
type EnvStore struct {
	Prefix string // e.g. "" (use the bare name) or "NILCORE_SECRET_"
}

// Name identifies the backend.
func (e EnvStore) Name() string { return "env" }

// Get reads Prefix+name from the environment.
func (e EnvStore) Get(name string) (string, error) {
	if v, ok := os.LookupEnv(e.Prefix + name); ok && v != "" {
		return v, nil
	}
	return "", fmt.Errorf("secret %q: %w", name, ErrNotFound)
}

// Set is unsupported: the environment store is read-only.
func (e EnvStore) Set(string, string) error { return ErrReadOnly }

// Delete is unsupported: the environment store is read-only.
func (e EnvStore) Delete(string) error { return ErrReadOnly }
