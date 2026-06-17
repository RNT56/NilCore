package worktree

import (
	"context"
	"strings"
	"testing"
)

// TestPinBranchSurvivesPrefixSweep is the durable-resume blocker test: the run-end
// branch sweep (DeleteBranches) reclaims the throwaway integrate/ prefix, but a tip
// pinned under resume/ must survive — keeping the merged commit reachable across a
// graceful restart. It also asserts the pin points at the exact SHA and is idempotent
// (re-pin moves it), and that DeleteBranch tears it down.
func TestPinBranchSurvivesPrefixSweep(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	repo := initRepo(t)

	// Two commits: the "tip" we want to preserve, then base advances past it.
	tip := commitFile(t, repo, "tip.txt", "merged work", "integration tip")
	_ = commitFile(t, repo, "later.txt", "more", "base advances")

	// Model the live integration tip as a throwaway integrate/ branch at `tip`, then
	// pin it under resume/ — the durable anchor.
	gitOut(t, repo, "branch", "integrate/abc", tip)
	if err := PinBranch(ctx, repo, "resume/run-1", tip); err != nil {
		t.Fatalf("PinBranch: %v", err)
	}
	if got := strings.TrimSpace(gitOut(t, repo, "rev-parse", "resume/run-1")); got != tip {
		t.Fatalf("pin points at %q, want tip %q", got, tip)
	}

	// The run-end sweep reclaims integrate/ (and the other throwaway prefixes) — the
	// exact prefixes buildStack's cleanup sweeps — but never touches resume/.
	for _, p := range []string{"task/", "rebase/", "integrate/", "read/"} {
		DeleteBranches(ctx, repo, p)
	}
	if got := strings.TrimSpace(gitOut(t, repo, "branch", "--list", "integrate/abc")); got != "" {
		t.Errorf("integrate/ branch should be swept, still present: %q", got)
	}
	if got := strings.TrimSpace(gitOut(t, repo, "rev-parse", "resume/run-1")); got != tip {
		t.Fatalf("resume/ pin did NOT survive the prefix sweep: %q (want %q)", got, tip)
	}
	// The tip commit is still reachable, so a worktree can be cut from it after cleanup.
	wt, err := CreateFrom(ctx, repo, "verify-resume/x", "vr-x", tip)
	if err != nil {
		t.Fatalf("CreateFrom preserved tip after sweep: %v", err)
	}
	_ = wt.Cleanup()

	// Re-pin moves the ref (git branch -f is idempotent); then DeleteBranch tears it down.
	newTip := commitFile(t, repo, "tip2.txt", "v2", "advance tip")
	if err := PinBranch(ctx, repo, "resume/run-1", newTip); err != nil {
		t.Fatalf("re-pin: %v", err)
	}
	if got := strings.TrimSpace(gitOut(t, repo, "rev-parse", "resume/run-1")); got != newTip {
		t.Errorf("re-pin did not move ref: %q want %q", got, newTip)
	}
	DeleteBranch(ctx, repo, "resume/run-1")
	if got := strings.TrimSpace(gitOut(t, repo, "branch", "--list", "resume/run-1")); got != "" {
		t.Errorf("DeleteBranch left the pin: %q", got)
	}
	// PinBranch validates its args.
	if err := PinBranch(ctx, repo, "", tip); err == nil {
		t.Error("PinBranch with empty branch should error")
	}
}
