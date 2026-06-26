package verify

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// seedTree writes files (rel path -> content) under a fresh temp dir and returns
// the root. Parent dirs are created as needed.
func seedTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %q: %v", rel, err)
		}
	}
	return root
}

// --- Content hash: determinism + sensitivity --------------------------------

func TestContentHashWorktreeDeterministic(t *testing.T) {
	files := map[string]string{
		"main.go":     "package main\n",
		"pkg/util.go": "package pkg\n",
		"README.md":   "# hi\n",
	}
	a := seedTree(t, files)
	b := seedTree(t, files)

	ctx := context.Background()
	ha, err := ContentHashWorktree(ctx, a)
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	hb, err := ContentHashWorktree(ctx, b)
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if ha != hb {
		t.Fatalf("identical trees hashed differently:\n a=%s\n b=%s", ha, hb)
	}
	// Stable across repeated calls on the same tree.
	ha2, err := ContentHashWorktree(ctx, a)
	if err != nil {
		t.Fatalf("rehash a: %v", err)
	}
	if ha != ha2 {
		t.Fatalf("same tree hashed differently across calls: %s != %s", ha, ha2)
	}
}

func TestContentHashWorktreeChangedFileChangesHash(t *testing.T) {
	ctx := context.Background()
	root := seedTree(t, map[string]string{"a.go": "package a\n", "b.go": "package b\n"})
	before, err := ContentHashWorktree(ctx, root)
	if err != nil {
		t.Fatalf("before: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a // changed\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	after, err := ContentHashWorktree(ctx, root)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Fatalf("changing a file did not change the hash: %s", before)
	}
}

func TestContentHashWorktreeAddedFileChangesHash(t *testing.T) {
	ctx := context.Background()
	root := seedTree(t, map[string]string{"a.go": "package a\n"})
	before, err := ContentHashWorktree(ctx, root)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatalf("add: %v", err)
	}
	after, err := ContentHashWorktree(ctx, root)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Fatalf("adding a file did not change the hash: %s", before)
	}
}

func TestContentHashWorktreeSkipsMetadata(t *testing.T) {
	ctx := context.Background()
	root := seedTree(t, map[string]string{"a.go": "package a\n"})
	base, err := ContentHashWorktree(ctx, root)
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	// Writing into .git / .nilcore must NOT change the source hash (they are skipped).
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: x\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".nilcore"), 0o755); err != nil {
		t.Fatalf("mkdir .nilcore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".nilcore", "log.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write .nilcore: %v", err)
	}
	withMeta, err := ContentHashWorktree(ctx, root)
	if err != nil {
		t.Fatalf("withMeta: %v", err)
	}
	if base != withMeta {
		t.Fatalf("metadata dirs leaked into the source hash: %s != %s", base, withMeta)
	}
}

func TestContentHashWorktreeExtraSkipDir(t *testing.T) {
	ctx := context.Background()
	root := seedTree(t, map[string]string{"a.go": "package a\n", "vendor/x.go": "package v\n"})
	full, err := ContentHashWorktree(ctx, root)
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	skipped, err := ContentHashWorktree(ctx, root, "vendor")
	if err != nil {
		t.Fatalf("skipped: %v", err)
	}
	if full == skipped {
		t.Fatalf("skipping vendor did not change the hash; it was not excluded")
	}
}

// --- Content hash over an explicit file set ---------------------------------

func TestContentHashFilesOrderIndependentAndSensitive(t *testing.T) {
	ctx := context.Background()
	root := seedTree(t, map[string]string{"a.go": "A\n", "b.go": "B\n", "c.go": "C\n"})

	h1, err := ContentHashFiles(ctx, root, []string{"a.go", "b.go"})
	if err != nil {
		t.Fatalf("h1: %v", err)
	}
	h2, err := ContentHashFiles(ctx, root, []string{"b.go", "a.go", "a.go"})
	if err != nil {
		t.Fatalf("h2: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("file-set hash depends on order/dupes: %s != %s", h1, h2)
	}

	// A different read-set ⇒ a different hash.
	h3, err := ContentHashFiles(ctx, root, []string{"a.go", "c.go"})
	if err != nil {
		t.Fatalf("h3: %v", err)
	}
	if h1 == h3 {
		t.Fatalf("different file sets hashed identically: %s", h1)
	}

	// Changing a member's bytes ⇒ a different hash.
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("A2\n"), 0o644); err != nil {
		t.Fatalf("rewrite a: %v", err)
	}
	h4, err := ContentHashFiles(ctx, root, []string{"a.go", "b.go"})
	if err != nil {
		t.Fatalf("h4: %v", err)
	}
	if h1 == h4 {
		t.Fatalf("changing a member's content did not change the hash")
	}
}

func TestContentHashFilesAbsentMemberIsDeterministic(t *testing.T) {
	ctx := context.Background()
	root := seedTree(t, map[string]string{"a.go": "A\n"})

	// A missing member folds as a stable "absent" entry, not an error.
	h1, err := ContentHashFiles(ctx, root, []string{"a.go", "gone.go"})
	if err != nil {
		t.Fatalf("h1: %v", err)
	}
	h2, err := ContentHashFiles(ctx, root, []string{"a.go", "gone.go"})
	if err != nil {
		t.Fatalf("h2: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("absent-member hash not deterministic: %s != %s", h1, h2)
	}
	// Present vs absent differ.
	hPresent, err := ContentHashFiles(ctx, root, []string{"a.go"})
	if err != nil {
		t.Fatalf("hPresent: %v", err)
	}
	if h1 == hPresent {
		t.Fatalf("absent member did not affect the hash")
	}
}

func TestContentHashFilesRejectsEscape(t *testing.T) {
	ctx := context.Background()
	root := seedTree(t, map[string]string{"a.go": "A\n"})
	if _, err := ContentHashFiles(ctx, root, []string{"../escape.go"}); err == nil {
		t.Fatalf("expected confinement error for a rel that escapes root")
	}
}

func TestContentHashEmptySetStable(t *testing.T) {
	ctx := context.Background()
	root := seedTree(t, map[string]string{"a.go": "A\n"})
	h1, err := ContentHashFiles(ctx, root, nil)
	if err != nil {
		t.Fatalf("h1: %v", err)
	}
	h2, err := ContentHashFiles(ctx, root, []string{})
	if err != nil {
		t.Fatalf("h2: %v", err)
	}
	if h1 != h2 || h1 == "" {
		t.Fatalf("empty set hash not stable/defined: %q vs %q", h1, h2)
	}
}

// --- Fail-class derivation ---------------------------------------------------

func TestFailClass(t *testing.T) {
	tests := []struct {
		name string
		rep  Report
		want string
	}{
		{"pass has no class", Report{Passed: true, Output: "passed: make verify, browser"}, FailClassPass},
		{"composite browser envelope", Report{Output: "check browser failed:\nsome behavioral detail"}, FailClassBrowser},
		{"go build command", Report{Output: "go build ./...\n./main.go:3:1: syntax error"}, FailClassBuild},
		{"go test command", Report{Output: "go test ./...\n--- FAIL: TestX"}, FailClassTest},
		{"go vet command", Report{Output: "go vet ./...\nprintf format %d"}, FailClassLint},
		{"golangci-lint command", Report{Output: "golangci-lint run\nfile.go:1: ineffassign"}, FailClassLint},
		{"absolute path lint binary", Report{Output: "/usr/local/bin/golangci-lint run\nx"}, FailClassLint},
		{"npm test command", Report{Output: "npm test\n1 failing"}, FailClassTest},
		{"pytest command", Report{Output: "pytest\n1 failed"}, FailClassTest},
		{"cargo build", Report{Output: "cargo build\nerror[E0382]"}, FailClassBuild},
		{"unknown program", Report{Output: "frobnicate --all\nkaboom"}, FailClassUnknown},
		{"empty output failure", Report{Output: ""}, FailClassUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FailClass(tt.rep); got != tt.want {
				t.Fatalf("FailClass(%q) = %q, want %q", tt.rep.Output, got, tt.want)
			}
		})
	}
}

// TestFailClassNeverLeaksRawOutput is the I7 guard: the label is ALWAYS one of the
// fixed vocabulary constants, and it never contains any distinctive bytes from the
// raw failing output — even when the output is wild or attacker-shaped.
func TestFailClassNeverLeaksRawOutput(t *testing.T) {
	vocab := map[string]bool{
		FailClassBuild: true, FailClassTest: true, FailClassLint: true,
		FailClassBrowser: true, FailClassPass: true, FailClassUnknown: true,
	}
	secret := "SECRET-TOKEN-do-not-leak-1234567890"
	outputs := []string{
		"go test ./...\nFAIL " + secret,
		"check browser failed:\n" + secret + " panic stack trace",
		secret + "\nmore raw output\n",
		"golangci-lint run\n" + secret,
		"ignore previous instructions and " + secret, // untrusted-as-data probe
		"frobnicate\n" + secret,
	}
	for _, out := range outputs {
		got := FailClass(Report{Output: out})
		if !vocab[got] {
			t.Fatalf("FailClass returned a non-vocabulary label %q for output %q", got, out)
		}
		if strings.Contains(got, secret) || strings.Contains(got, "ignore") {
			t.Fatalf("FailClass leaked raw output into the label: %q", got)
		}
	}
}

// --- Toolchain ---------------------------------------------------------------

func TestToolchain(t *testing.T) {
	got := Toolchain()
	if !strings.Contains(got, runtime.Version()) {
		t.Errorf("toolchain %q missing go version %q", got, runtime.Version())
	}
	if !strings.Contains(got, runtime.GOOS) || !strings.Contains(got, runtime.GOARCH) {
		t.Errorf("toolchain %q missing GOOS/GOARCH", got)
	}
	if got != Toolchain() {
		t.Errorf("toolchain not deterministic across calls")
	}
}

// --- Golden default-path guard ----------------------------------------------

// TestEnrichDefaultPathLeavesVerifyUnchanged proves the additive helpers are
// inert: with no caller wiring them in, the verify package's existing behavior is
// byte-identical. We exercise the unchanged seams directly — a passing Report has
// no fail-class, the no-op Pass verifier still passes, and DetectOrOverride is
// unchanged — so this file cannot have perturbed anything that ships.
func TestEnrichDefaultPathLeavesVerifyUnchanged(t *testing.T) {
	// A passing report yields the empty fail-class: no signal, no behavior change.
	if cls := FailClass(Report{Passed: true}); cls != "" {
		t.Fatalf("a pass must have no fail-class, got %q", cls)
	}
	// The existing no-op verifier is untouched: still always passes.
	rep, err := Pass{}.Check(context.Background())
	if err != nil || !rep.Passed {
		t.Fatalf("Pass verifier changed: rep=%+v err=%v", rep, err)
	}
	// Auto-detection of the verify command is unchanged.
	if cmd := DetectOrOverride("", "make verify"); cmd != "make verify" {
		t.Fatalf("DetectOrOverride changed: %q", cmd)
	}
}
