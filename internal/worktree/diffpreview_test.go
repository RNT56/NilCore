package worktree

import (
	"context"
	"strings"
	"testing"
)

// DiffPreview must report a kept branch's change against HEAD as stat + diff,
// bounded, and degrade cleanly on the edge cases (no change / unknown branch).
func TestDiffPreview(t *testing.T) {
	requireGit(t)
	repo := initRepo(t)
	commitFile(t, repo, "a.txt", "base\n", "base")

	// A kept branch one commit ahead of HEAD.
	gitOut(t, repo, "branch", "kept/x")
	gitOut(t, repo, "checkout", "-q", "kept/x")
	commitFile(t, repo, "a.txt", "changed\n", "edit on kept")
	gitOut(t, repo, "checkout", "-q", "-")

	got, err := DiffPreview(context.Background(), repo, "kept/x", 0)
	if err != nil {
		t.Fatalf("DiffPreview: %v", err)
	}
	if !strings.Contains(got, "a.txt") || !strings.Contains(got, "+changed") {
		t.Fatalf("preview missing stat/diff content:\n%s", got)
	}

	// Bounded: a tiny cap clips on a line boundary and marks the elision.
	clipped, err := DiffPreview(context.Background(), repo, "kept/x", 32)
	if err != nil {
		t.Fatalf("DiffPreview clipped: %v", err)
	}
	if len(clipped) > 32+len("\n… (diff truncated)") || !strings.Contains(clipped, "truncated") {
		t.Fatalf("preview not clamped: %d bytes\n%s", len(clipped), clipped)
	}
}

func TestDiffPreviewNoChange(t *testing.T) {
	requireGit(t)
	repo := initRepo(t)
	commitFile(t, repo, "a.txt", "base\n", "base")
	gitOut(t, repo, "branch", "kept/same") // same tip as HEAD ⇒ nothing to land

	got, err := DiffPreview(context.Background(), repo, "kept/same", 0)
	if err != nil {
		t.Fatalf("DiffPreview: %v", err)
	}
	if got != "" {
		t.Fatalf("identical branch produced a preview:\n%s", got)
	}
}

func TestDiffPreviewUnknownBranch(t *testing.T) {
	requireGit(t)
	repo := initRepo(t)
	if _, err := DiffPreview(context.Background(), repo, "kept/gone", 0); err == nil {
		t.Fatal("an unresolvable branch must be a clear error, not an empty preview")
	}
	if _, err := DiffPreview(context.Background(), repo, "", 0); err == nil {
		t.Fatal("an empty branch must error")
	}
}
