package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/integrate"
	"nilcore/internal/verify"
	"nilcore/internal/worktree"
)

type passCheck struct{}

func (passCheck) Check(context.Context) (verify.Report, error) {
	return verify.Report{Passed: true, Output: "ok"}, nil
}

// TestIntegrateTipSurvivesForLiveRead is the end-to-end regression for the live-read
// fix: buildIntegrateFunc must KEEP (Release, not Cleanup) the integrate/<suffix> tip
// branch it returns, so the supervisor's RefreshRead can re-point the read worktree at
// it and read the integrated tree. Before the fix the branch was deleted before the
// caller saw it, so the headline path silently no-op'd.
func TestIntegrateTipSurvivesForLiveRead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()

	// A repo with a base commit and a verified task branch carrying feat.txt.
	repo := t.TempDir()
	rgit(t, repo, "init", "-q")
	rgit(t, repo, "-c", "user.email=t@nilcore.local", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "base")
	base := rgit(t, repo, "rev-parse", "HEAD")
	rgit(t, repo, "checkout", "-q", "-b", "task/t1", base)
	if err := os.WriteFile(filepath.Join(repo, "feat.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rgit(t, repo, "add", "-A")
	rgit(t, repo, "-c", "user.email=t@nilcore.local", "-c", "user.name=t", "commit", "-q", "-m", "t1")
	rgit(t, repo, "checkout", "-q", base) // detach so task/t1 is free to merge/checkout

	intr := &integrate.Integrator{
		BaseRepo: repo,
		NewEnv:   func(string) integrate.Env { return integrate.Env{Verifier: passCheck{}} },
		Log:      openTestLog(t),
	}
	integrateFn := buildIntegrateFunc(intr)

	branch, results, err := integrateFn(ctx, []integrate.MergeItem{{ID: "t1", Branch: "task/t1"}})
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if branch == "" || len(results) != 1 || !results[0].Verified {
		t.Fatalf("expected one verified merge onto a tip branch, got branch=%q results=%+v", branch, results)
	}

	// THE REGRESSION: the returned tip branch must still resolve — Cleanup would have
	// deleted it before we got here, breaking RefreshRead.
	rp := exec.Command("git", "-C", repo, "rev-parse", "--verify", branch)
	if out, rerr := rp.CombinedOutput(); rerr != nil {
		t.Fatalf("integration tip branch %q was deleted before the caller could read it (the live-read bug): %v\n%s", branch, rerr, out)
	}

	// And RefreshRead's mechanism works against it: a read worktree re-pointed at the
	// tip sees the integrated tree (feat.txt is present).
	readWt, err := worktree.CreateFrom(ctx, repo, "read/probe", "rd", "HEAD")
	if err != nil {
		t.Fatalf("read worktree: %v", err)
	}
	defer func() { _ = readWt.Cleanup() }()
	if err := readWt.Checkout(ctx, branch); err != nil {
		t.Fatalf("RefreshRead Checkout(%s) failed — tip not readable: %v", branch, err)
	}
	files, err := readWt.ListFiles(ctx, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if !strings.Contains(files, "feat.txt") {
		t.Errorf("re-pointed read tree missing the integrated file; ListFiles=%q", files)
	}
}
