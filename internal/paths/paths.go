// Package paths resolves per-OS config, data, and cache directories for NilCore,
// so one cross-compiled binary behaves correctly on macOS and Linux. It follows
// the XDG base-directory spec on Linux and ~/Library/Application Support on
// macOS. Stdlib only (invariant I6).
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// app is the per-user directory name under each base.
const app = "nilcore"

// ConfigDir is where NilCore reads/writes configuration — and, crucially, where the
// file-vault master key + encrypted secret store live (see cmd/nilcore/main.go).
//
//	Linux:  $XDG_CONFIG_HOME/nilcore              (default ~/.config/nilcore)
//	macOS:  ~/Library/Application Support/nilcore  (os.UserConfigDir)
//
// On macOS os.UserConfigDir() returns ~/Library/Application Support, the SAME base
// DataDir uses. That "collapse" is deliberate and correct here: macOS has no XDG-style
// config/data split, and Application Support is the platform-conventional home for both.
// We do NOT relocate config to ~/Library/Preferences — that would change the resolved
// path for EXISTING installs, orphaning an already-provisioned secrets vault/master key
// (MasterKeyFromFile would then mint a FRESH key and silently render every stored secret
// undecryptable on upgrade). Keeping ConfigDir stable at Application Support preserves
// those secrets. Linux/XDG behaviour is unchanged.
func ConfigDir() (string, error) {
	base, err := os.UserConfigDir() // XDG on Linux; ~/Library/Application Support on macOS
	if err != nil {
		return "", fmt.Errorf("resolve config dir: %w", err)
	}
	return filepath.Join(base, app), nil
}

// DataDir is where NilCore stores persistent data (event log, SQLite store).
//
//	Linux:  $XDG_DATA_HOME/nilcore  (default ~/.local/share/nilcore)
//	macOS:  ~/Library/Application Support/nilcore
func DataDir() (string, error) {
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", app), nil
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, app), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".local", "share", app), nil
}

// EnsureDir creates dir (and parents) with 0o700 perms and returns it, so callers
// can do `dir, err := EnsureDir(ConfigDir())`-style chaining for a ready path.
func EnsureDir(dir string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create dir %s: %w", dir, err)
	}
	return dir, nil
}
