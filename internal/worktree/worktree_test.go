package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo makes a throwaway git repo with one commit so HEAD exists.
func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@nilcore.local", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return repo
}

func TestCreateAndCleanup(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	wt, err := Create(context.Background(), repo, "P1-T01")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := os.Stat(wt.Path()); err != nil {
		t.Fatalf("worktree path missing: %v", err)
	}
	if wt.Branch() != "task/P1-T01" {
		t.Errorf("Branch() = %q, want task/P1-T01", wt.Branch())
	}
	// It must be a file inside the worktree (checked-out HEAD has a .git file).
	if _, err := os.Stat(filepath.Join(wt.Path(), ".git")); err != nil {
		t.Errorf("worktree .git missing: %v", err)
	}

	list := gitOut(t, repo, "worktree", "list")
	if !strings.Contains(list, wt.Path()) {
		t.Fatalf("worktree not registered:\n%s", list)
	}

	if err := wt.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(wt.Path()); !os.IsNotExist(err) {
		t.Fatalf("worktree path still exists after cleanup (err=%v)", err)
	}
	// Branch should be gone.
	if branches := gitOut(t, repo, "branch", "--list", "task/P1-T01"); strings.TrimSpace(branches) != "" {
		t.Errorf("branch not deleted: %q", branches)
	}
	// Idempotent.
	if err := wt.Cleanup(); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
}

func TestCreateEmptyTaskID(t *testing.T) {
	if _, err := Create(context.Background(), t.TempDir(), ""); err == nil {
		t.Fatal("expected error for empty task id")
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// commitFile writes name=body into repo, commits it, and returns the new SHA.
func commitFile(t *testing.T, repo, name, body, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	gitOut(t, repo, "add", "-A")
	gitOut(t, repo, "-c", "user.email=t@nilcore.local", "-c", "user.name=t", "commit", "-q", "-m", msg)
	return strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD"))
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// CreateFrom must branch off an arbitrary committish, not just HEAD. We make a
// repo with two commits, branch off the FIRST, and assert the worktree sees the
// old state — proving the start-point is honored.
func TestCreateFromArbitrarySHA(t *testing.T) {
	requireGit(t)
	repo := initRepo(t)
	first := commitFile(t, repo, "a.txt", "v1", "add a")
	_ = commitFile(t, repo, "b.txt", "v2", "add b") // advance HEAD past `first`

	wt, err := CreateFrom(context.Background(), repo, "branch/old", "old", first)
	if err != nil {
		t.Fatalf("CreateFrom: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()

	head, err := wt.Head(context.Background())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head != first {
		t.Errorf("worktree HEAD = %q, want start-point %q", head, first)
	}
	// b.txt was added after `first`, so it must NOT be present in this checkout.
	if _, err := os.Stat(filepath.Join(wt.Path(), "b.txt")); !os.IsNotExist(err) {
		t.Errorf("b.txt present in worktree off old SHA (err=%v)", err)
	}
}

// An unresolvable start-point must be a clear error, never a panic, and must not
// leave a leaked worktree behind. Covers both an unknown SHA and the greenfield
// empty-HEAD case.
func TestCreateFromUnresolvableStartPoint(t *testing.T) {
	requireGit(t)

	t.Run("unknown SHA", func(t *testing.T) {
		repo := initRepo(t)
		_, err := CreateFrom(context.Background(), repo, "branch/x", "x", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
		if err == nil {
			t.Fatal("expected error for unknown start-point SHA")
		}
	})

	t.Run("greenfield empty HEAD", func(t *testing.T) {
		repo := t.TempDir() // `git init` with NO commits → HEAD does not resolve
		gitOut(t, repo, "init", "-q")
		_, err := CreateFrom(context.Background(), repo, "branch/g", "g", "HEAD")
		if err == nil {
			t.Fatal("expected error creating worktree in an empty-HEAD repo")
		}
	})
}

// Commit on a clean tree is a no-op: it returns the current HEAD and changed=false
// rather than failing with git's "nothing to commit".
func TestCommitCleanTree(t *testing.T) {
	requireGit(t)
	repo := initRepo(t)

	wt, err := Create(context.Background(), repo, "P2-T01")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()

	before, err := wt.Head(context.Background())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	sha, changed, err := wt.Commit(context.Background(), "nothing to do")
	if err != nil {
		t.Fatalf("Commit on clean tree: %v", err)
	}
	if changed {
		t.Error("changed = true on a clean tree, want false")
	}
	if sha != before {
		t.Errorf("clean-tree Commit sha = %q, want unchanged HEAD %q", sha, before)
	}
}

// Commit on a dirty tree stages everything and returns the new SHA with changed=true.
func TestCommitDirtyTree(t *testing.T) {
	requireGit(t)
	repo := initRepo(t)

	wt, err := Create(context.Background(), repo, "P2-T01-dirty")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()

	before, err := wt.Head(context.Background())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path(), "new.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	sha, changed, err := wt.Commit(context.Background(), "add new")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !changed {
		t.Error("changed = false after writing a file, want true")
	}
	if sha == before {
		t.Errorf("Commit sha unchanged %q after a real commit", sha)
	}
	now, _ := wt.Head(context.Background())
	if now != sha {
		t.Errorf("returned sha %q != HEAD %q", sha, now)
	}
}

// DiffStat reports what changed in the worktree since it was created: the
// changed-file name-status list and the --stat summary, bounded, between the
// pinned create-time baseline and the worker's committed HEAD. A new file added
// and committed must show up; an unchanged worktree reports "".
func TestDiffStat(t *testing.T) {
	requireGit(t)
	repo := initRepo(t)

	wt, err := Create(context.Background(), repo, "diff-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()

	// A fresh worktree off HEAD with no work done: nothing changed.
	empty, err := wt.DiffStat(context.Background(), 0)
	if err != nil {
		t.Fatalf("DiffStat (clean): %v", err)
	}
	if empty != "" {
		t.Errorf("DiffStat on an unchanged worktree = %q, want empty", empty)
	}

	// Add a file and commit it; the diff (baseline → HEAD) must name it.
	if err := os.WriteFile(filepath.Join(wt.Path(), "feature.go"), []byte("package x\n\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, changed, err := wt.Commit(context.Background(), "add feature"); err != nil || !changed {
		t.Fatalf("Commit: changed=%v err=%v", changed, err)
	}

	report, err := wt.DiffStat(context.Background(), 0)
	if err != nil {
		t.Fatalf("DiffStat: %v", err)
	}
	if !strings.Contains(report, "feature.go") {
		t.Errorf("DiffStat missing the changed file:\n%s", report)
	}
	if !strings.Contains(report, "Changed files:") {
		t.Errorf("DiffStat missing the changed-file header:\n%s", report)
	}
	// The --stat summary carries an insertions count for the new file.
	if !strings.Contains(report, "insertion") && !strings.Contains(report, "+") {
		t.Errorf("DiffStat missing the --stat summary:\n%s", report)
	}
}

// DiffStat is bounded: a worktree with many large changed files yields a report
// no larger than the byte cap, ending with the truncation marker — never a raw,
// unbounded dump.
func TestDiffStatBounded(t *testing.T) {
	requireGit(t)
	repo := initRepo(t)

	wt, err := Create(context.Background(), repo, "diff-bound")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()

	// Many files so the name-status list alone overflows a small cap.
	body := strings.Repeat("x", 200)
	for i := 0; i < 50; i++ {
		name := filepath.Join(wt.Path(), "file_"+strings.Repeat("a", i+1)+".txt")
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if _, _, err := wt.Commit(context.Background(), "many files"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	const cap = 256
	report, err := wt.DiffStat(context.Background(), cap)
	if err != nil {
		t.Fatalf("DiffStat: %v", err)
	}
	// The cap is on the diff body; the truncation marker is a small bounded suffix.
	if len(report) > cap+len("\n… (diff truncated)") {
		t.Errorf("DiffStat report len %d exceeds cap %d (+marker)", len(report), cap)
	}
	if !strings.Contains(report, "truncated") {
		t.Errorf("a bounded report over the cap must carry the truncation marker:\n%s", report)
	}
}

// DiffStat is taken against the create-time baseline, not the moving HEAD: even
// after a commit advances the branch, the report still reflects everything done
// since the worktree was cut (so a multi-commit worker's full change is reported).
func TestDiffStatAgainstCreateBaseline(t *testing.T) {
	requireGit(t)
	repo := initRepo(t)

	wt, err := Create(context.Background(), repo, "diff-baseline")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()

	// Two separate commits in the worktree.
	if err := os.WriteFile(filepath.Join(wt.Path(), "one.txt"), []byte("1"), 0o644); err != nil {
		t.Fatalf("write one: %v", err)
	}
	if _, _, err := wt.Commit(context.Background(), "first"); err != nil {
		t.Fatalf("Commit one: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path(), "two.txt"), []byte("2"), 0o644); err != nil {
		t.Fatalf("write two: %v", err)
	}
	if _, _, err := wt.Commit(context.Background(), "second"); err != nil {
		t.Fatalf("Commit two: %v", err)
	}

	report, err := wt.DiffStat(context.Background(), 0)
	if err != nil {
		t.Fatalf("DiffStat: %v", err)
	}
	// Both files (across both commits) must appear: the baseline is the create
	// point, not the previous commit.
	if !strings.Contains(report, "one.txt") || !strings.Contains(report, "two.txt") {
		t.Errorf("DiffStat did not span both commits since create:\n%s", report)
	}
}

// The hardening clamp must keep a repo-authored .git/hooks/post-checkout from
// executing on the host: CreateFrom runs `worktree add` (which would fire
// post-checkout) under HardenArgs core.hooksPath=/dev/null. We plant a hook that
// writes a sentinel file; after Create the sentinel must NOT exist.
func TestCreateDoesNotRunRepoHook(t *testing.T) {
	requireGit(t)
	repo := initRepo(t)

	sentinel := filepath.Join(t.TempDir(), "pwned")
	hooksDir := filepath.Join(repo, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	hook := "#!/bin/sh\ntouch " + sentinel + "\n"
	if err := os.WriteFile(filepath.Join(hooksDir, "post-checkout"), []byte(hook), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	wt, err := Create(context.Background(), repo, "hook-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()

	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("post-checkout hook executed on the host (hardening clamp bypassed)")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error: %v", err)
	}
}

// TestReleaseKeepsBranchForDependent is the DependsOn-propagation regression: a
// worker's worktree is Release'd (checkout removed, branch KEPT) so a dependent
// can cut a new worktree from that branch and see the dependency's committed code;
// a worker cut from HEAD does not. DeleteBranches then sweeps the kept branches.
func TestReleaseKeepsBranchForDependent(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	repo := initRepo(t)

	// Dependency t1: commit a file, then Release (checkout gone, branch kept).
	wt1, err := CreateFrom(ctx, repo, "task/t1", "t1", "HEAD")
	if err != nil {
		t.Fatalf("CreateFrom t1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt1.Path(), "dep.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wt1.Commit(ctx, "t1 work"); err != nil {
		t.Fatalf("commit t1: %v", err)
	}
	if err := wt1.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(wt1.Path()); err == nil {
		t.Fatal("Release left the checkout directory present")
	}

	// A dependent cut from t1's surviving branch SEES dep.go.
	wt2, err := CreateFrom(ctx, repo, "task/t2", "t2", "task/t1")
	if err != nil {
		t.Fatalf("CreateFrom task/t1 (the branch must survive Release): %v", err)
	}
	defer func() { _ = wt2.Cleanup() }()
	if _, err := os.Stat(filepath.Join(wt2.Path(), "dep.go")); err != nil {
		t.Errorf("dependent should see the dependency's dep.go: %v", err)
	}

	// A worker cut from HEAD does NOT see it (the bug this fixes).
	wt3, err := CreateFrom(ctx, repo, "task/t3", "t3", "HEAD")
	if err != nil {
		t.Fatalf("CreateFrom HEAD: %v", err)
	}
	defer func() { _ = wt3.Cleanup() }()
	if _, err := os.Stat(filepath.Join(wt3.Path(), "dep.go")); err == nil {
		t.Error("a worker cut from HEAD must NOT see the dependency's dep.go")
	}

	// DeleteBranches sweeps the kept branches: cutting from task/t1 then fails.
	DeleteBranches(ctx, repo, "task/")
	if _, err := CreateFrom(ctx, repo, "task/t4", "t4", "task/t1"); err == nil {
		t.Error("DeleteBranches should have removed task/t1 (CreateFrom from it must now fail)")
	}
}
