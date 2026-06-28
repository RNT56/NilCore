package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/kernel"
)

func TestDecomposePlan(t *testing.T) {
	cases := []struct {
		name, goal string
		want       []string
	}{
		{"empty", "", nil},
		{"single", "fix the panic in auth.go", []string{"fix the panic in auth.go"}},
		{"and", "add a login form and wire the logout button", []string{"add a login form", "wire the logout button"}},
		{"semicolons", "add tests; update docs; bump version", []string{"add tests", "update docs", "bump version"}},
		{"comma-and", "scaffold the API, and add a healthcheck", []string{"scaffold the API", "add a healthcheck"}},
		{"numbered list", "1. add a model\n2. add a handler\n3. add a test", []string{"add a model", "add a handler", "add a test"}},
		{"multiline collapsing to one item", "the one thing to do\n\n", []string{"the one thing to do"}},
		{"dash list", "- parser\n- printer", []string{"parser", "printer"}},
		{"trailing period stripped", "do a thing.", []string{"do a thing"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decomposePlan(c.goal)
			if len(got) != len(c.want) {
				t.Fatalf("decomposePlan(%q) = %v, want %v", c.goal, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("decomposePlan(%q)[%d] = %q, want %q", c.goal, i, got[i], c.want[i])
				}
			}
		})
	}
}

func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "user.email=t@nilcore.local", "-c", "user.name=t"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// addChildBranch creates branch off base with one commit adding file=content, then
// returns to base — simulating what a verified KeepBranch child run leaves behind.
func addChildBranch(t *testing.T, repo, base, branch, file, content string) {
	t.Helper()
	git(t, repo, "checkout", "-q", "-b", branch, base)
	if err := os.WriteFile(filepath.Join(repo, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "add "+file)
	git(t, repo, "checkout", "-q", base)
}

func baseBranch(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestIntegrateBranchesMergesVerified(t *testing.T) {
	repo := initEquivGitRepo(t)
	base := baseBranch(t, repo)
	addChildBranch(t, repo, base, "child-a", "a.txt", "alpha\n")
	addChildBranch(t, repo, base, "child-b", "b.txt", "beta\n")

	alwaysGreen := func(context.Context, string) (bool, error) { return true, nil }
	children := []childResult{
		{subGoal: "do a", branch: "child-a", verified: true},
		{subGoal: "do b", branch: "child-b", verified: true},
	}
	wt, res, err := integrateBranches(context.Background(), repo, "integ-1", children, alwaysGreen)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()
	if res.merged != 2 || !res.verified || res.dropped != 0 {
		t.Fatalf("res = %+v, want merged=2 verified=true dropped=0", res)
	}
	// Both files are present in the integrated tip.
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(wt.Path(), f)); err != nil {
			t.Errorf("%s missing from integrated tip: %v", f, err)
		}
	}
}

func TestIntegrateBranchesDropsRedMerge(t *testing.T) {
	repo := initEquivGitRepo(t)
	base := baseBranch(t, repo)
	addChildBranch(t, repo, base, "child-good", "good.txt", "ok\n")
	addChildBranch(t, repo, base, "child-bad", "bad.txt", "boom\n")

	// Verifier fails iff the tree contains bad.txt — so merging child-bad turns it red.
	verify := func(_ context.Context, dir string) (bool, error) {
		_, err := os.Stat(filepath.Join(dir, "bad.txt"))
		return err != nil, nil // green only when bad.txt is absent
	}
	children := []childResult{
		{subGoal: "good", branch: "child-good", verified: true},
		{subGoal: "bad", branch: "child-bad", verified: true},
	}
	wt, res, err := integrateBranches(context.Background(), repo, "integ-2", children, verify)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()
	if res.merged != 1 || res.dropped != 1 || !res.verified {
		t.Fatalf("res = %+v, want merged=1 dropped=1 verified=true (bad child reverted)", res)
	}
	if _, err := os.Stat(filepath.Join(wt.Path(), "bad.txt")); err == nil {
		t.Error("bad.txt must have been reverted out of the integrated tip")
	}
	if _, err := os.Stat(filepath.Join(wt.Path(), "good.txt")); err != nil {
		t.Error("good.txt should remain in the integrated tip")
	}
}

func TestIntegrateBranchesDropsConflict(t *testing.T) {
	repo := initEquivGitRepo(t)
	base := baseBranch(t, repo)
	// Both children add the SAME file with different content ⇒ add/add conflict on the 2nd.
	addChildBranch(t, repo, base, "child-1", "shared.txt", "from one\n")
	addChildBranch(t, repo, base, "child-2", "shared.txt", "from two\n")

	alwaysGreen := func(context.Context, string) (bool, error) { return true, nil }
	children := []childResult{
		{subGoal: "one", branch: "child-1", verified: true},
		{subGoal: "two", branch: "child-2", verified: true},
	}
	wt, res, err := integrateBranches(context.Background(), repo, "integ-3", children, alwaysGreen)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()
	if res.merged != 1 || res.dropped != 1 {
		t.Fatalf("res = %+v, want merged=1 dropped=1 (the conflicting child dropped)", res)
	}
}

func TestIntegrateBranchesSkipsUnverifiedChildren(t *testing.T) {
	repo := initEquivGitRepo(t)
	base := baseBranch(t, repo)
	addChildBranch(t, repo, base, "child-ok", "ok.txt", "ok\n")

	alwaysGreen := func(context.Context, string) (bool, error) { return true, nil }
	children := []childResult{
		{subGoal: "ok", branch: "child-ok", verified: true},
		{subGoal: "failed", branch: "", verified: false}, // a child that never verified
	}
	wt, res, err := integrateBranches(context.Background(), repo, "integ-4", children, alwaysGreen)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	defer func() { _ = wt.Cleanup() }()
	if res.merged != 1 || res.dropped != 1 {
		t.Fatalf("res = %+v, want merged=1 dropped=1 (unverified child skipped)", res)
	}
}

// TestDecomposeEnvelopeEndToEnd drives the whole recursive preset through kernel.Run with
// a fake child runner (creates a real branch per sub-goal) + a fake verifier: it proves
// plan → run each child → integrate composes, the integrated tip carries every merged
// child, and the kernel Outcome reflects the integrator's verdict (I2).
func TestDecomposeEnvelopeEndToEnd(t *testing.T) {
	repo := initEquivGitRepo(t)
	base := baseBranch(t, repo)
	runChild := func(_ context.Context, subGoal, taskID string) (string, bool, error) {
		br := "child-" + taskID
		addChildBranch(t, repo, base, br, taskID+".txt", subGoal+"\n")
		return br, true, nil
	}
	alwaysGreen := func(context.Context, string) (bool, error) { return true, nil }

	env, st := decomposeEnvelope("root", repo, runChild, alwaysGreen, 8, nil)
	out, err := kernel.Run(context.Background(), env,
		kernel.Node{ID: "root", Goal: "add a model and add a handler and add a test"})
	if err != nil {
		t.Fatalf("decompose Run: %v", err)
	}
	if !out.Verified {
		t.Fatalf("decompose outcome not verified: %+v", out)
	}
	if st.res.merged != 3 {
		t.Fatalf("merged %d, want 3 sub-goals", st.res.merged)
	}
	defer func() { _ = st.wt.Cleanup() }()
	for _, f := range []string{"root-1.txt", "root-2.txt", "root-3.txt"} {
		if _, err := os.Stat(filepath.Join(st.wt.Path(), f)); err != nil {
			t.Errorf("integrated tip missing %s: %v", f, err)
		}
	}
}

// TestIntegrateBranchesFinalVerifyError: the FINAL re-verify (after every per-merge check)
// is a distinct path — if it errors, the integrator must surface the error and clean up the
// integration worktree (return nil), never leak it or claim a verdict.
func TestIntegrateBranchesFinalVerifyError(t *testing.T) {
	repo := initEquivGitRepo(t)
	base := baseBranch(t, repo)
	addChildBranch(t, repo, base, "child-a", "a.txt", "alpha\n")

	calls := 0
	verify := func(context.Context, string) (bool, error) {
		calls++
		if calls >= 2 { // call 1 = the per-merge check; call 2 = the final re-verify
			return false, errors.New("verifier crashed")
		}
		return true, nil
	}
	children := []childResult{{subGoal: "a", branch: "child-a", verified: true}}
	wt, _, err := integrateBranches(context.Background(), repo, "integ-fv", children, verify)
	if err == nil {
		t.Fatal("a final-verify error must surface as an error")
	}
	if wt != nil {
		t.Fatal("on a final-verify error the integration worktree must be cleaned up (nil returned)")
	}
}
