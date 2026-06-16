package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Checkout re-points a worktree (detached) at another ref, forcing its working tree
// to match — how a long-lived read worktree tracks the moving integration tip.
func TestCheckoutRepointsTree(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	// dep1 has a.txt; dep2 has b.txt — divergent off the same base.
	repo := twoBranches(t, "a.txt", "A\n", "b.txt", "B\n")

	wt, err := CreateFrom(ctx, repo, "read/x", "rd", "dep1")
	if err != nil {
		t.Fatalf("CreateFrom dep1: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()

	// Starts at dep1: a.txt present, b.txt absent.
	if _, err := os.Stat(filepath.Join(wt.Path(), "a.txt")); err != nil {
		t.Fatalf("dep1 worktree missing a.txt: %v", err)
	}

	// Re-point to dep2: now b.txt present, a.txt gone.
	if err := wt.Checkout(ctx, "dep2"); err != nil {
		t.Fatalf("Checkout dep2: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt.Path(), "b.txt")); err != nil {
		t.Errorf("after Checkout(dep2) b.txt missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt.Path(), "a.txt")); !os.IsNotExist(err) {
		t.Errorf("after Checkout(dep2) a.txt should be gone, stat err=%v", err)
	}
	// HEAD is detached (not on a branch) so a re-point never conflicts with a branch
	// checked out elsewhere.
	if br := strings.TrimSpace(gitOut(t, wt.Path(), "rev-parse", "--abbrev-ref", "HEAD")); br != "HEAD" {
		t.Errorf("Checkout should leave a DETACHED HEAD, got branch %q", br)
	}

	// ListFiles reports the current tree's tracked files (structure for grounding).
	files, err := wt.ListFiles(ctx, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if !strings.Contains(files, "b.txt") || strings.Contains(files, "a.txt") {
		t.Errorf("ListFiles after dep2 = %q, want b.txt only", files)
	}
}
