package vcache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHashWorktreeDeterministic: the same tree hashes to the same value across
// repeated calls, regardless of walk order.
func TestHashWorktreeDeterministic(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main")
	writeFile(t, root, "sub/a.txt", "alpha")
	writeFile(t, root, "sub/b.txt", "beta")

	h := HashWorktree(root)
	h1, err := h(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h2, err := h(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("hash not deterministic: %q != %q", h1, h2)
	}
	if h1 == "" {
		t.Error("hash of a non-empty tree should be non-empty")
	}
}

// TestHashWorktreeChangeSensitive: editing any file's bytes changes the hash, so a
// changed worktree never reuses a prior key.
func TestHashWorktreeChangeSensitive(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main")
	writeFile(t, root, "sub/a.txt", "alpha")

	before, _ := HashWorktree(root)(context.Background())
	writeFile(t, root, "sub/a.txt", "alpha-edited")
	after, _ := HashWorktree(root)(context.Background())
	if before == after {
		t.Error("editing a file must change the worktree hash")
	}
}

// TestHashWorktreeNewFileChangesHash: adding a file changes the hash even if no
// existing file is touched.
func TestHashWorktreeNewFileChangesHash(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main")

	before, _ := HashWorktree(root)(context.Background())
	writeFile(t, root, "new.go", "package new")
	after, _ := HashWorktree(root)(context.Background())
	if before == after {
		t.Error("adding a file must change the worktree hash")
	}
}

// TestHashWorktreeSkipsMetadata: the .git and .nilcore directories are excluded, so
// the cache is not self-invalidating (folding the event log into the key would make
// every recorded pass change the very hash that keys it).
func TestHashWorktreeSkipsMetadata(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main")

	base, _ := HashWorktree(root)(context.Background())

	// Mutating .git / .nilcore must NOT move the source hash.
	writeFile(t, root, ".git/HEAD", "ref: refs/heads/main")
	writeFile(t, root, ".nilcore/events.jsonl", `{"seq":0}`)
	after, _ := HashWorktree(root)(context.Background())
	if base != after {
		t.Errorf("metadata dirs must not affect the source hash: %q != %q", base, after)
	}
}

// TestHashWorktreeMissingRoot: a non-existent root is a surfaced error, never a
// silent empty hash (which would let an unhashed tree spuriously "match").
func TestHashWorktreeMissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := HashWorktree(missing)(context.Background()); err == nil {
		t.Error("hashing a missing root must error, not return an empty hash")
	}
}

// TestHashWorktreeCancel: a cancelled context aborts the walk with the ctx error,
// honoring the ctx-first cancellation contract.
func TestHashWorktreeCancel(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := HashWorktree(root)(ctx); err == nil {
		t.Error("a cancelled context should abort hashing")
	}
}
