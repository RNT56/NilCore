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
