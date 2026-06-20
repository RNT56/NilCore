package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// sampleArtifact builds a small, fully-populated artifact for round-trip tests.
func sampleArtifact(id string) *Artifact {
	at := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	return &Artifact{
		ID:        id,
		Kind:      KindReport,
		Title:     "store round-trip",
		CreatedAt: at,
		Claims: []Claim{
			{
				ID:    id + "-c1",
				Field: "revenue_fy2024",
				Evidence: Evidence{
					Value:    "123456789",
					Verifier: "finance.sec_fact",
					Status:   StatusPass,
				},
			},
		},
	}
}

// TestArtifactStoreRoundTrip is the happy path: Write places JSON at the fixed
// .nilcore/artifacts/<id>.json under root, and Read returns the same Artifact.
func TestArtifactStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	orig := sampleArtifact("rpt-1")

	if err := Write(root, orig); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// The file must land at exactly the fixed out-of-band path.
	want := filepath.Join(root, ".nilcore", "artifacts", "rpt-1.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("artifact not at fixed path %q: %v", want, err)
	}

	got, err := Read(root, "rpt-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.ID != orig.ID || got.Kind != orig.Kind || got.Title != orig.Title {
		t.Fatalf("Read mismatch: got %+v", got)
	}
	if len(got.Claims) != 1 || got.Claims[0].Evidence.Status != StatusPass {
		t.Fatalf("claim not round-tripped: %+v", got.Claims)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", got.SchemaVersion, SchemaVersion)
	}
}

// TestArtifactStoreRejectsBadID asserts an id with a separator or .. is rejected by
// BOTH Write and Read, and that Write creates no escape file.
func TestArtifactStoreRejectsBadID(t *testing.T) {
	root := t.TempDir()
	bad := []string{
		"",
		"..",
		".",
		"a/b",
		"../escape",
		"sub/../x",
		".hidden",
	}
	for _, id := range bad {
		t.Run("id="+id, func(t *testing.T) {
			a := sampleArtifact(id)
			if err := Write(root, a); err == nil {
				t.Fatalf("Write(%q) should be rejected", id)
			}
			if _, err := Read(root, id); err == nil {
				t.Fatalf("Read(%q) should be rejected", id)
			}
		})
	}

	// No file anywhere outside the (still-empty or nonexistent) artifacts dir was
	// created by the rejected writes — in particular no "escape" file at the root.
	if _, err := os.Stat(filepath.Join(root, "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an escape file was created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an escape file was created outside root: %v", err)
	}
}

// TestArtifactStoreSymlinkRefused plants a symlink at the target path and asserts
// Read fails closed (O_NOFOLLOW via worktreefs) rather than following it.
func TestArtifactStoreSymlinkRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root := t.TempDir()

	// Create the artifacts dir and a sensitive file the symlink would point at.
	dir := filepath.Join(root, ".nilcore", "artifacts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	secret := filepath.Join(root, "secret.json")
	if err := os.WriteFile(secret, []byte(`{"id":"leak"}`), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	// Plant evil.json -> secret.json at the target path.
	target := filepath.Join(dir, "evil.json")
	if err := os.Symlink(secret, target); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := Read(root, "evil"); err == nil {
		t.Fatal("Read of a symlinked target should fail closed (O_NOFOLLOW), not follow the link")
	}
}

// TestArtifactStoreNotFound asserts a missing file is a typed ErrNotFound,
// distinct from a parse error.
func TestArtifactStoreNotFound(t *testing.T) {
	root := t.TempDir()
	_, err := Read(root, "absent")
	if err == nil {
		t.Fatal("Read of a missing artifact should error")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing artifact should be ErrNotFound, got %v", err)
	}
}

// TestArtifactStoreParseError asserts corrupt on-disk JSON returns a parse error
// (never a silent zero-value) and is NOT confused with ErrNotFound.
func TestArtifactStoreParseError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".nilcore", "artifacts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := Read(root, "bad")
	if err == nil {
		t.Fatalf("Read of corrupt JSON should error, got %+v", got)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("a parse error must be distinct from ErrNotFound")
	}
}

// TestArtifactStoreWriteNil asserts a nil artifact is rejected.
func TestArtifactStoreWriteNil(t *testing.T) {
	if err := Write(t.TempDir(), nil); err == nil {
		t.Fatal("Write(nil) should error")
	}
}

// TestArtifactStoreOverwrite asserts Write is an atomic overwrite: a second Write
// of the same id replaces the first and Read sees the new value.
func TestArtifactStoreOverwrite(t *testing.T) {
	root := t.TempDir()
	a := sampleArtifact("ovr")
	if err := Write(root, a); err != nil {
		t.Fatalf("Write#1: %v", err)
	}
	a.Title = "second"
	a.Claims[0].Evidence.Status = StatusFail
	if err := Write(root, a); err != nil {
		t.Fatalf("Write#2: %v", err)
	}
	got, err := Read(root, "ovr")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Title != "second" || got.Claims[0].Evidence.Status != StatusFail {
		t.Fatalf("overwrite not applied: %+v", got)
	}
}
