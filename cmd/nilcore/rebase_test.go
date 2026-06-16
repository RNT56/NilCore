package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func rgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// repoTwoBranches makes a repo with a base commit and two diverged branches:
// dep1 sets file1=body1, dep2 sets file2=body2. HEAD ends detached on the base.
func repoTwoBranches(t *testing.T, file1, body1, file2, body2 string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	rgit(t, repo, "init", "-q")
	rgit(t, repo, "-c", "user.email=t@nilcore.local", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "base")
	base := rgit(t, repo, "rev-parse", "HEAD")
	commit := func(branch, file, body string) {
		rgit(t, repo, "checkout", "-q", "-b", branch, base)
		if err := os.WriteFile(filepath.Join(repo, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		rgit(t, repo, "add", "-A")
		rgit(t, repo, "-c", "user.email=t@nilcore.local", "-c", "user.name=t", "commit", "-q", "-m", branch)
	}
	commit("dep1", file1, body1)
	commit("dep2", file2, body2)
	rgit(t, repo, "checkout", "-q", base)
	return repo
}

// mergedBaseTip of two non-conflicting dep branches returns a throwaway branch
// whose tree is the UNION of both deps' files (the Phase-2 multi-dep re-base).
func TestMergedBaseTipUnion(t *testing.T) {
	repo := repoTwoBranches(t, "a.txt", "AAA\n", "b.txt", "BBB\n")
	branch, conflict, err := mergedBaseTip(context.Background(), repo, "super.t3", []string{"dep1", "dep2"})
	if err != nil || conflict {
		t.Fatalf("non-conflicting union: conflict=%t err=%v", conflict, err)
	}
	if branch == "" || !strings.HasPrefix(branch, "rebase/super.t3-") {
		t.Fatalf("want a rebase/super.t3-<seq> branch, got %q", branch)
	}
	tree := rgit(t, repo, "ls-tree", "-r", "--name-only", branch)
	for _, f := range []string{"a.txt", "b.txt"} {
		if !strings.Contains(tree, f) {
			t.Errorf("merged tip missing %s; tree:\n%s", f, tree)
		}
	}
}

// A conflicting dep set degrades to ("", conflict=true, nil): the caller falls back
// to base HEAD and the spawn never fails. No rebase/ branch is left behind.
func TestMergedBaseTipConflictFallback(t *testing.T) {
	repo := repoTwoBranches(t, "c.txt", "X\n", "c.txt", "Y\n")
	branch, conflict, err := mergedBaseTip(context.Background(), repo, "super.t3", []string{"dep1", "dep2"})
	if err != nil {
		t.Fatalf("a dep-branch conflict must be conflict=true, not an error: %v", err)
	}
	if !conflict || branch != "" {
		t.Fatalf("want graceful conflict fallback (branch=\"\" conflict=true), got branch=%q conflict=%t", branch, conflict)
	}
	if b := rgit(t, repo, "branch", "--list", "rebase/*"); strings.TrimSpace(b) != "" {
		t.Errorf("conflict path left a rebase/ branch behind: %q", b)
	}
}

// Fewer than two refs is a no-op: the caller uses the single-dep BaseRef / HEAD.
func TestMergedBaseTipFewerThanTwo(t *testing.T) {
	repo := repoTwoBranches(t, "a.txt", "A\n", "b.txt", "B\n")
	for _, refs := range [][]string{nil, {"dep1"}} {
		branch, conflict, err := mergedBaseTip(context.Background(), repo, "x", refs)
		if branch != "" || conflict || err != nil {
			t.Errorf("refs=%v: want no-op (\"\",false,nil), got (%q,%t,%v)", refs, branch, conflict, err)
		}
	}
}
