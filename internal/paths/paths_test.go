package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConfigDir(t *testing.T) {
	dir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	if filepath.Base(dir) != app {
		t.Errorf("ConfigDir = %q, want it to end in %q", dir, app)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(dir, filepath.Join("Library", "Application Support")) {
		t.Errorf("macOS ConfigDir = %q, want under Library/Application Support", dir)
	}
}

// ConfigDir hosts the secrets vault + master key, so its resolved path must stay STABLE
// across releases. On Linux, XDG keeps config and data distinct. On macOS they DELIBERATELY
// share ~/Library/Application Support (os.UserConfigDir): macOS has no XDG-style split, and
// relocating config to ~/Library/Preferences would orphan an already-provisioned secrets
// vault on upgrade (a fresh master key would be minted, silently losing every stored
// secret). This test guards against re-introducing that relocation.
func TestConfigDirIsStableSecretsHome(t *testing.T) {
	cfg, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	data, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if runtime.GOOS == "darwin" {
		// Both under Application Support (the deliberate, secret-preserving collapse).
		if !strings.Contains(cfg, filepath.Join("Library", "Application Support", app)) {
			t.Errorf("macOS ConfigDir = %q, want under Library/Application Support/%s (relocating it orphans the secrets vault)", cfg, app)
		}
		if !strings.Contains(data, filepath.Join("Library", "Application Support", app)) {
			t.Errorf("macOS DataDir = %q, want under Library/Application Support/%s", data, app)
		}
		return
	}
	// Linux/XDG: config and data remain distinct.
	if cfg == data {
		t.Errorf("ConfigDir and DataDir collapsed to the same path %q on non-darwin", cfg)
	}
}

func TestDataDirXDG(t *testing.T) {
	if runtime.GOOS == "darwin" {
		dir, err := DataDir()
		if err != nil {
			t.Fatalf("DataDir: %v", err)
		}
		if !strings.Contains(dir, filepath.Join("Library", "Application Support", app)) {
			t.Errorf("macOS DataDir = %q", dir)
		}
		return
	}
	// Linux & others: XDG_DATA_HOME takes precedence.
	t.Setenv("XDG_DATA_HOME", "/tmp/xdgdata")
	dir, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if want := filepath.Join("/tmp/xdgdata", app); dir != want {
		t.Errorf("DataDir = %q, want %q", dir, want)
	}
}

func TestEnsureDir(t *testing.T) {
	target := filepath.Join(t.TempDir(), "a", "b", "nilcore")
	got, err := EnsureDir(target, nil)
	if err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if got != target {
		t.Errorf("EnsureDir returned %q, want %q", got, target)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("dir not created: %v", err)
	}
	// Propagates an upstream error unchanged.
	if _, err := EnsureDir("", os.ErrPermission); err == nil {
		t.Error("EnsureDir should propagate an incoming error")
	}
}
