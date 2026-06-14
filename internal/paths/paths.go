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

// ConfigDir is where NilCore reads/writes configuration.
//
//	Linux:  $XDG_CONFIG_HOME/nilcore  (default ~/.config/nilcore)
//	macOS:  ~/Library/Application Support/nilcore
func ConfigDir() (string, error) {
	base, err := os.UserConfigDir() // already XDG on Linux, App Support on macOS
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

// CacheDir is where NilCore stores disposable caches.
//
//	Linux:  $XDG_CACHE_HOME/nilcore  (default ~/.cache/nilcore)
//	macOS:  ~/Library/Caches/nilcore
func CacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve cache dir: %w", err)
	}
	return filepath.Join(base, app), nil
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
