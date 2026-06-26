package onboard_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/graapprove"
	"nilcore/internal/onboard"
)

func TestValidate_AutoApproveOptional(t *testing.T) {
	// Default-off: a config with no AutoApprove block validates exactly as before.
	if err := (onboard.Config{Version: onboard.CurrentConfigVersion}).Validate(); err != nil {
		t.Fatalf("empty config (no auto_approve) should validate, got %v", err)
	}

	// A valid preset envelope validates.
	env, err := graapprove.Preset("conservative")
	if err != nil {
		t.Fatalf("preset: %v", err)
	}
	if err := (onboard.Config{Version: onboard.CurrentConfigVersion, AutoApprove: &env}).Validate(); err != nil {
		t.Fatalf("config with a valid auto_approve should validate, got %v", err)
	}
}

func TestValidate_InvalidAutoApproveFailsClosed(t *testing.T) {
	// An ill-formed envelope (MinSuccesses 0 — a blank trust bar) must error, never
	// be silently accepted as "auto-approve everything".
	bad := graapprove.Envelope{Classes: []graapprove.ClassClause{{Type: "open-pr"}}}
	err := (onboard.Config{Version: onboard.CurrentConfigVersion, AutoApprove: &bad}).Validate()
	if err == nil {
		t.Fatalf("invalid auto_approve envelope must fail validation")
	}
	if !strings.Contains(err.Error(), "auto_approve") {
		t.Errorf("error should attribute the failure to auto_approve, got %v", err)
	}
}

func TestLoad_LegacyConfigWithoutAutoApprove(t *testing.T) {
	// An existing pre-Phase-16 config (no auto_approve key) still loads cleanly
	// under the bumped config version — the v2→v3 migration is a no-op and the
	// field defaults to nil (auto-approval off).
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c, err := onboard.Load(path)
	if err != nil {
		t.Fatalf("legacy v2 config should load, got %v", err)
	}
	if c.AutoApprove != nil {
		t.Errorf("a config without the block must default to nil AutoApprove (off)")
	}
}

func TestSaveLoad_AutoApproveRoundTrips(t *testing.T) {
	env, _ := graapprove.Preset("standard")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := (onboard.Config{AutoApprove: &env}).Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	c, err := onboard.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.AutoApprove == nil || len(c.AutoApprove.Classes) == 0 {
		t.Errorf("auto_approve envelope did not round-trip through save/load: %+v", c.AutoApprove)
	}
}
