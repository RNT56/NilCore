package agent_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
)

// writingBackend writes a file into the task worktree so a kept branch has a real
// commit (something ahead of base to PR).
type writingBackend struct{}

func (writingBackend) Name() string { return "writing" }
func (writingBackend) Run(_ context.Context, t backend.Task) (backend.Result, error) {
	_ = os.WriteFile(filepath.Join(t.Dir, "new.txt"), []byte("hello\n"), 0o644)
	return backend.Result{Backend: "writing", Summary: "wrote a file", SelfClaimed: true}, nil
}

func gitTrim(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}

func keepBranchOrch(repo string, keep, verified bool) *agent.Orchestrator {
	return &agent.Orchestrator{
		BaseRepo: repo,
		NewEnv: func(string) agent.Env {
			return agent.Env{Backend: writingBackend{}, Verifier: &fakeVerifier{passed: verified}}
		},
		Router:     agent.SingleRouter{},
		Spawner:    agent.NoSpawner{},
		KeepBranch: keep,
	}
}

// KeepBranch + verified success: the branch is preserved (Released, not Cleanup'd),
// reported in Outcome.Branch, exists in the repo, and is ahead of HEAD (carries the
// committed work) — the precondition for the gated trigger→PR push (D4).
func TestKeepBranchPreservesVerifiedBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	out, err := keepBranchOrch(repo, true, true).Execute(context.Background(), backend.Task{ID: "keep-1", Goal: "add a file"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Verified || out.Branch == "" {
		t.Fatalf("KeepBranch+verified must report a branch; got verified=%v branch=%q", out.Verified, out.Branch)
	}
	if gitTrim(t, repo, "rev-parse", "--verify", out.Branch) == "" {
		t.Fatalf("kept branch %q does not exist in the repo", out.Branch)
	}
	if ahead := gitTrim(t, repo, "rev-list", "--count", "HEAD.."+out.Branch); ahead == "0" || ahead == "" {
		t.Errorf("kept branch should be ahead of HEAD (carry the work), got %q", ahead)
	}
}

// Default mode (KeepBranch=false) is byte-identical to before: no branch reported,
// and the worktree + its branch are cleaned up — no leak.
func TestDefaultDisposableLeavesNoBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	out, err := keepBranchOrch(repo, false, true).Execute(context.Background(), backend.Task{ID: "disp-1", Goal: "add a file"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Branch != "" {
		t.Errorf("default mode must not report a branch, got %q", out.Branch)
	}
	if b := gitTrim(t, repo, "branch", "--list", "task/*"); b != "" {
		t.Errorf("default mode left a branch behind: %q", b)
	}
}

// KeepBranch=true but verify FAILS: the branch must NOT be preserved — a failed run
// is disposable, so no branch leaks and nothing is offered for a PR.
func TestKeepBranchVerifyFailLeavesNoBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	out, err := keepBranchOrch(repo, true, false).Execute(context.Background(), backend.Task{ID: "fail-1", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Verified {
		t.Fatal("expected not verified")
	}
	if out.Branch != "" {
		t.Errorf("verify-fail must not preserve a branch, got %q", out.Branch)
	}
	if b := gitTrim(t, repo, "branch", "--list", "task/*"); b != "" {
		t.Errorf("verify-fail under KeepBranch left a branch: %q", b)
	}
}

// raceRecoverEnv returns a NewEnv whose FIRST call (the single attempt) fails verification
// and whose later calls (the race copies) pass — so a KeepBranch+RaceN run fails the cheap
// attempt, escalates to the race, and the race recovers. writingBackend commits a file so a
// kept winner branch is genuinely ahead of HEAD.
func raceRecoverEnv() func(string) agent.Env {
	calls := 0
	return func(string) agent.Env {
		calls++
		return agent.Env{Backend: writingBackend{}, Verifier: &fakeVerifier{passed: calls > 1}}
	}
}

// KeepBranch + a first-attempt verify failure that a RACE recovers: the WINNING race
// candidate's branch must be preserved (committed, kept, reported in Outcome.Branch), and
// ONLY the winner — the losing race worktree/branch is still cleaned. Before the fix, D4 was
// honored only on the non-race single path, so a chat/TUI drive with KeepBranch+RaceN>1 that
// failed once then won the race reported "verified" while keeping NOTHING (/diff and /apply
// had no branch and the verified diff was deleted).
func TestKeepBranchPreservesRaceWinnerBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	orch := &agent.Orchestrator{
		BaseRepo:   repo,
		NewEnv:     raceRecoverEnv(),
		Router:     agent.SingleRouter{},
		Spawner:    agent.NoSpawner{},
		KeepBranch: true,
		RaceN:      2,
	}
	out, err := orch.Execute(context.Background(), backend.Task{ID: "racekeep-1", Goal: "add a file"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Verified {
		t.Fatal("the race copies pass; expected a verified outcome")
	}
	if out.Branch == "" {
		t.Fatal("KeepBranch + race-win must preserve the WINNER's branch; got empty Branch")
	}
	// The kept branch exists and carries the committed work (ahead of HEAD) — the
	// precondition for /diff and /apply.
	if gitTrim(t, repo, "rev-parse", "--verify", out.Branch) == "" {
		t.Fatalf("kept race branch %q does not exist in the repo", out.Branch)
	}
	if ahead := gitTrim(t, repo, "rev-list", "--count", "HEAD.."+out.Branch); ahead == "0" || ahead == "" {
		t.Errorf("kept race branch should be ahead of HEAD (carry the work), got %q", ahead)
	}
	// EXACTLY the winner survives under race/: the loser was disposed. (out.Branch is the
	// raw race/<id>-<i> tip; the chat/serve delivery layer re-homes it under nilcore/kept/.)
	survivors := strings.Fields(gitTrim(t, repo, "for-each-ref", "--format=%(refname:short)", "refs/heads/race/"))
	if len(survivors) != 1 || survivors[0] != out.Branch {
		t.Errorf("want exactly the winner %q surviving under race/, got %v (loser not cleaned?)", out.Branch, survivors)
	}
}

// KeepBranch=false: a race win is byte-identical to before — no branch is reported and
// EVERY race worktree/branch is disposed (the default disposable mode never leaks a branch).
func TestRaceWinnerNoBranchWithoutKeepBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		NewEnv:   raceRecoverEnv(),
		Router:   agent.SingleRouter{},
		Spawner:  agent.NoSpawner{},
		RaceN:    2, // race, but KeepBranch stays false
	}
	out, err := orch.Execute(context.Background(), backend.Task{ID: "racenokeep-1", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Verified {
		t.Fatal("race copies pass; expected verified")
	}
	if out.Branch != "" {
		t.Errorf("KeepBranch=false must report no branch, got %q", out.Branch)
	}
	if b := gitTrim(t, repo, "for-each-ref", "--format=%(refname:short)", "refs/heads/race/"); b != "" {
		t.Errorf("KeepBranch=false must dispose EVERY race branch, got %q", b)
	}
}
