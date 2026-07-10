package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The vcache key must cover every operator toggle that changes WHAT the composite
// verifier checks. A pass recorded with a check off must not replay as green once
// the operator turns that check on — that would skip the gate entirely (I2).
func TestBehavioralVerifierIDCoversEveryToggle(t *testing.T) {
	const cmd = "go build ./... && go test ./..."

	base := behavioralVerifierID(cmd)

	toggles := []struct{ env, val string }{
		{"NILCORE_BROWSER_VERIFY", "npm run e2e"},
		{"NILCORE_EVIDENCE_VERIFY", "1"},
		{"NILCORE_VERIFY_PACKS", "web,finance"},
		{"NILCORE_EVIDENCE_MAX_AGE", "24h"},
	}
	for _, tg := range toggles {
		t.Setenv(tg.env, tg.val)
		got := behavioralVerifierID(cmd)
		if got == base {
			t.Errorf("%s changed the verdict surface but not the cache key (%q) — a green recorded without it would replay", tg.env, got)
		}
		os.Unsetenv(tg.env)
	}

	// A different project command must key differently.
	if behavioralVerifierID("make verify") == base {
		t.Error("different verify command produced the same cache key")
	}
	// Identical config must key identically (the cache must still hit).
	if behavioralVerifierID(cmd) != base {
		t.Error("identical config produced different cache keys; the cache would never hit")
	}
	// The resolved command stays legible as a prefix.
	if !strings.HasPrefix(base, cmd+"#") {
		t.Errorf("cache key %q lost its legible command prefix", base)
	}
}

// The evidence legs read .nilcore/artifacts/*.json. Rewriting an artifact to carry
// failing claims must change the content hash, or the old chain-verified pass
// replays as green over the new claims.
func TestVerifiedContentHashCoversArtifactsWhenEvidenceOn(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	artDir := filepath.Join(root, ".nilcore", "artifacts")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatal(err)
	}
	art := filepath.Join(artDir, "rpt-1.json")
	if err := os.WriteFile(art, []byte(`{"id":"rpt-1","claims":[{"status":"pass"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	box := &fakeVerifierBox{dir: root}
	ctx := context.Background()

	t.Setenv("NILCORE_EVIDENCE_VERIFY", "1")
	before, err := verifiedContentHash(ctx, box)
	if err != nil {
		t.Fatal(err)
	}

	// Rewrite the artifact: same worktree source, different claims.
	if err := os.WriteFile(art, []byte(`{"id":"rpt-1","claims":[{"status":"fail"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := verifiedContentHash(ctx, box)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("rewriting an artifact did not change the content hash — the old pass would replay over new claims (I2)")
	}
}

// With evidence verification off the artifacts are not read, so they must stay out
// of the key (a run that merely writes one should not bust the cache).
func TestVerifiedContentHashIgnoresArtifactsWhenEvidenceOff(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	artDir := filepath.Join(root, ".nilcore", "artifacts")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatal(err)
	}

	box := &fakeVerifierBox{dir: root}
	ctx := context.Background()
	os.Unsetenv("NILCORE_EVIDENCE_VERIFY")

	before, err := verifiedContentHash(ctx, box)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "rpt-1.json"), []byte(`{"id":"rpt-1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := verifiedContentHash(ctx, box)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Error("artifacts changed the hash while evidence verification is off; they are not read on that path")
	}
}

// The tiered fast path may only arm when the `go test` LEG itself is full-module.
func TestTieredSoundReadsTheGoTestLegOnly(t *testing.T) {
	sound := []string{
		"go test ./...",
		"go build ./... && go test ./...",
		"go vet ./... && go test -short ./...",
	}
	for _, c := range sound {
		if !tieredSound(c) {
			t.Errorf("tieredSound(%q) = false, want true", c)
		}
	}

	unsound := []string{
		"go build ./... && go test ./pkg", // "./..." lives on the BUILD leg only
		"make verify",
		"cargo test",    // contains the bytes "go test"
		"go test ./pkg", // single package
		"npm test",      //
	}
	for _, c := range unsound {
		if tieredSound(c) {
			t.Errorf("tieredSound(%q) = true, want false (a scoped red would not be a provable subset)", c)
		}
	}
}

// The scoped run must replicate the flags that decide WHICH tests run, and must not
// graft another leg's flags onto `go test`.
func TestGoTestFlagsScopedToTestLeg(t *testing.T) {
	cases := map[string]string{
		"go test ./...":                              "",
		"go test -short -race ./...":                 "-short -race",
		"go test -run TestFoo ./...":                 "-run TestFoo",
		"go test -run=TestFoo -skip=TestBar ./...":   "-run=TestFoo -skip=TestBar",
		"go test -tags integration ./...":            "-tags integration",
		"go build -tags prod ./... && go test ./...": "", // -tags belongs to the build leg
		"go build ./... && go test -short ./...":     "-short",
	}
	for cmd, want := range cases {
		if got := goTestFlags(cmd); got != want {
			t.Errorf("goTestFlags(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// THE regression test for the eager-evidence bug. In a real run the verifier is
// constructed right after the worktree is cut — before the backend writes any
// artifact (.nilcore/artifacts/ is gitignored, so a fresh worktree has none). If
// evidence legs are discovered at construction, the composite freezes with zero of
// them and the run's own artifact is never checked, despite the operator enabling
// NILCORE_EVIDENCE_VERIFY.
func TestEvidenceLegsDiscoveredAfterConstruction(t *testing.T) {
	t.Setenv("NILCORE_EVIDENCE_VERIFY", "1")
	t.Setenv("NILCORE_BROWSER_VERIFY", "")

	box := &fakeVerifierBox{dir: t.TempDir(), exit: 0}

	// Construct FIRST, exactly as envFactory/orchestratorVerifier do.
	v := behavioralVerifier(box, "true")

	bc, ok := v.(behavioralComposite)
	if !ok {
		t.Fatalf("evidence enabled must yield a behavioralComposite, got %T", v)
	}
	if n := len(bc.compose()); n != 1 {
		t.Fatalf("before any artifact exists the composite has %d legs, want 1 (checks only)", n)
	}

	// The backend now writes the artifact, mid-run.
	writeURLArtifact(t, box.dir, "rep", "https://example.com")

	named := bc.compose()
	if len(named) < 3 {
		t.Fatalf("after the run wrote an artifact the composite has %d legs, want >=3 "+
			"(checks + schema + evidence) — evidence verification is dead on this path", len(named))
	}
	if named[0].Name != "checks" {
		t.Errorf("Named[0] = %q, want the build verifier first (I2)", named[0].Name)
	}
	if !strings.HasPrefix(named[len(named)-1].Name, "evidence") {
		t.Errorf("last leg = %q, want the evidence verifier", named[len(named)-1].Name)
	}
}

// A malformed staleness window must refuse to start, not silently disable itself.
func TestValidateEvidenceMaxAgeFailsClosedOnTypo(t *testing.T) {
	t.Setenv("NILCORE_EVIDENCE_MAX_AGE", "24hours")
	if err := validateEvidenceMaxAge(); err == nil {
		t.Fatal("a malformed duration must be a boot error, not a silent 'staleness off'")
	}
	t.Setenv("NILCORE_EVIDENCE_MAX_AGE", "-1h")
	if err := validateEvidenceMaxAge(); err == nil {
		t.Fatal("a negative duration must be a boot error")
	}
	t.Setenv("NILCORE_EVIDENCE_MAX_AGE", "24h")
	if err := validateEvidenceMaxAge(); err != nil {
		t.Fatalf("valid duration rejected: %v", err)
	}
	os.Unsetenv("NILCORE_EVIDENCE_MAX_AGE")
	if err := validateEvidenceMaxAge(); err != nil {
		t.Fatalf("unset must be nil: %v", err)
	}
}
