package agent_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/verify"
)

// --- FIX 1: a self-suspend must PRESERVE committed work, not delete it ---

// committingSuspendBackend commits a file in its worktree, then self-suspends (the
// `sleep` tool). The committed work must SURVIVE the nap — the old behavior `git
// branch -D`'d the task branch on the suspend path and destroyed it.
type committingSuspendBackend struct{}

func (committingSuspendBackend) Name() string { return "commit-suspend" }
func (committingSuspendBackend) Run(_ context.Context, t backend.Task) (backend.Result, error) {
	_ = os.WriteFile(filepath.Join(t.Dir, "work.txt"), []byte("committed before sleep\n"), 0o644)
	for _, args := range [][]string{
		{"add", "-A"},
		{"-c", "user.email=a@nilcore.local", "-c", "user.name=a", "commit", "-q", "-m", "work before sleep"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = t.Dir
		_ = cmd.Run()
	}
	return backend.Result{Backend: "commit-suspend", Summary: "committed, now sleeping"}, backend.ErrSuspended
}

// A drive that commits then sleeps must keep its committed work reachable under a
// preserved ref (reported in Outcome.Branch and recorded in the durable checkpoint),
// NOT have it destroyed by the worktree's branch cleanup.
func TestSuspendPreservesCommittedWork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	ckpt, s := newCheckpoint(t)
	fv := &fakeVerifier{passed: true}
	orch := &agent.Orchestrator{
		BaseRepo:   repo,
		NewEnv:     func(string) agent.Env { return agent.Env{Backend: committingSuspendBackend{}, Verifier: fv} },
		Router:     agent.SingleRouter{},
		Spawner:    agent.NoSpawner{},
		Checkpoint: ckpt,
	}

	out, err := orch.Execute(context.Background(), backend.Task{ID: "suspend-keep-1", Goal: "work then sleep"})
	if !errors.Is(err, backend.ErrSuspended) {
		t.Fatalf("Execute = %v, want ErrSuspended", err)
	}
	if fv.checked {
		t.Error("a suspended drive must NOT run the verifier")
	}
	// The committed work is preserved under a ref reported in Outcome.Branch.
	if out.Branch == "" {
		t.Fatal("suspend must preserve the committed work under a branch (Outcome.Branch), got empty")
	}
	if gitTrim(t, repo, "rev-parse", "--verify", out.Branch) == "" {
		t.Fatalf("preserved suspend branch %q does not exist — committed work was destroyed", out.Branch)
	}
	if ahead := gitTrim(t, repo, "rev-list", "--count", "HEAD.."+out.Branch); ahead == "0" || ahead == "" {
		t.Errorf("preserved branch must carry the pre-sleep commit (ahead of HEAD), got %q", ahead)
	}
	// The durable checkpoint is "suspended" (Resume skips it) AND records the branch so a
	// resume can find the work.
	rec, gerr := s.GetTask(context.Background(), "suspend-keep-1")
	if gerr != nil {
		t.Fatalf("GetTask: %v", gerr)
	}
	if rec.Status != "suspended" {
		t.Errorf("status = %q, want suspended", rec.Status)
	}
	if !strings.Contains(rec.Detail, out.Branch) {
		t.Errorf("suspend checkpoint Detail %q must record the preserved branch %q", rec.Detail, out.Branch)
	}
}

// --- FIX 2: a race-won result under KeepBranch must carry a surviving branch ---

// A verify-fail escalates to a best-of-N race; when a race copy passes under KeepBranch,
// the WINNER's branch must be preserved (Released, not Cleanup'd) and reported — the
// same "keep a verifier-green one" contract the non-race path honors.
func TestRaceWonKeepBranchPreservesWinnerBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	var calls int
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		NewEnv: func(string) agent.Env {
			calls++
			// The single attempt (call 1) fails verification, forcing the race; the race
			// copies pass, so route.Race recovers a verifier-green winner.
			return agent.Env{Backend: writingBackend{}, Verifier: &fakeVerifier{passed: calls > 1}}
		},
		Router:     agent.SingleRouter{},
		Spawner:    agent.NoSpawner{},
		RaceN:      2,
		KeepBranch: true,
	}

	out, err := orch.Execute(context.Background(), backend.Task{ID: "racekeep-1", Goal: "add a file"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Verified {
		t.Fatal("the race had verifier-passing copies; expected Verified")
	}
	if out.Branch == "" {
		t.Fatal("a race-won KeepBranch outcome must carry a non-empty Branch (its work preserved)")
	}
	if gitTrim(t, repo, "rev-parse", "--verify", out.Branch) == "" {
		t.Fatalf("preserved race-winner branch %q does not exist in the repo", out.Branch)
	}
	if ahead := gitTrim(t, repo, "rev-list", "--count", "HEAD.."+out.Branch); ahead == "0" || ahead == "" {
		t.Errorf("race-winner branch should carry the work (ahead of HEAD), got %q", ahead)
	}
}

// KeepBranch=false is unaffected: a race-won result reports no branch and leaves no
// race branch behind (byte-identical disposable cleanup).
func TestRaceWonDefaultLeavesNoBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	var calls int
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		NewEnv: func(string) agent.Env {
			calls++
			return agent.Env{Backend: writingBackend{}, Verifier: &fakeVerifier{passed: calls > 1}}
		},
		Router:  agent.SingleRouter{},
		Spawner: agent.NoSpawner{},
		RaceN:   2,
	}
	out, err := orch.Execute(context.Background(), backend.Task{ID: "racedisp-1", Goal: "add a file"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Verified {
		t.Fatal("expected a recovered verified race result")
	}
	if out.Branch != "" {
		t.Errorf("default (non-KeepBranch) race must report no branch, got %q", out.Branch)
	}
	if b := gitTrim(t, repo, "branch", "--list", "race/*"); b != "" {
		t.Errorf("default race left a branch behind: %q", b)
	}
}

// --- FIX 3: a cancelled drive yields a clean interrupted outcome, not a verify fault ---

// cancelDuringBackend cancels the task ctx mid-drive (an operator /cancel, SIGTERM, or
// a deadline landing while the model runs), then returns the clean interrupted Result
// a well-behaved backend returns — NOT an error.
type cancelDuringBackend struct{ cancel context.CancelFunc }

func (cancelDuringBackend) Name() string { return "cancel-drive" }
func (b cancelDuringBackend) Run(ctx context.Context, _ backend.Task) (backend.Result, error) {
	b.cancel() // the interrupt lands mid-drive
	return backend.Result{Backend: "cancel-drive", Summary: "interrupted: " + context.Canceled.Error()}, nil
}

// On a cancelled ctx the orchestrator must SKIP the final verify (running it on the dead
// ctx would fault with "context canceled") and return a clean interrupted Outcome.
func TestCancelledDriveSkipsVerifyAndReturnsClean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	fv := &fakeVerifier{passed: true}
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		NewEnv:   func(string) agent.Env { return agent.Env{Backend: cancelDuringBackend{cancel: cancel}, Verifier: fv} },
		Router:   agent.SingleRouter{},
		Spawner:  agent.NoSpawner{},
	}

	out, err := orch.Execute(ctx, backend.Task{ID: "cancel-1", Goal: "x"})
	if err != nil {
		t.Fatalf("a cancelled drive must yield a clean interrupted outcome, got error: %v", err)
	}
	if fv.checked {
		t.Error("a cancelled drive must NOT run the final verify on the dead ctx")
	}
	if out.Verified {
		t.Error("a cancelled drive is not verified (it was cut short, not completed)")
	}
}

// --- FIX 4: a cleanly-errored task must be finalized, not left "running" for Resume ---

type erroringBackend struct{}

func (erroringBackend) Name() string { return "err" }
func (erroringBackend) Run(context.Context, backend.Task) (backend.Result, error) {
	return backend.Result{Backend: "err"}, errors.New("backend exploded")
}

type erroringVerifier struct{ checked bool }

func (v *erroringVerifier) Check(context.Context) (verify.Report, error) {
	v.checked = true
	return verify.Report{}, errors.New("verifier infra broke")
}

// A backend FAULT (on a live ctx) must finalize the durable checkpoint "failed" so a
// restart's Resume does not re-drive a task the live process already decided.
func TestBackendErrorFinalizesCheckpoint(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	ckpt, s := newCheckpoint(t)
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		NewEnv: func(string) agent.Env {
			return agent.Env{Backend: erroringBackend{}, Verifier: &fakeVerifier{passed: true}}
		},
		Router:     agent.SingleRouter{},
		Spawner:    agent.NoSpawner{},
		Checkpoint: ckpt,
	}
	if _, err := orch.Execute(context.Background(), backend.Task{ID: "err-1", Goal: "x"}); err == nil {
		t.Fatal("expected a backend error")
	}
	rec, gerr := s.GetTask(context.Background(), "err-1")
	if gerr != nil {
		t.Fatalf("GetTask: %v", gerr)
	}
	if rec.Status != "failed" {
		t.Errorf("a cleanly-errored task must be finalized 'failed' (not left 'running' to be re-driven on serve boot); got %q", rec.Status)
	}
}

// A verify FAULT (infra error) must likewise finalize the checkpoint "failed".
func TestVerifyErrorFinalizesCheckpoint(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	ckpt, s := newCheckpoint(t)
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		NewEnv: func(string) agent.Env {
			return agent.Env{Backend: &fakeBackend{name: "ok"}, Verifier: &erroringVerifier{}}
		},
		Router:     agent.SingleRouter{},
		Spawner:    agent.NoSpawner{},
		Checkpoint: ckpt,
	}
	if _, err := orch.Execute(context.Background(), backend.Task{ID: "verr-1", Goal: "x"}); err == nil {
		t.Fatal("expected a verify error")
	}
	rec, gerr := s.GetTask(context.Background(), "verr-1")
	if gerr != nil {
		t.Fatalf("GetTask: %v", gerr)
	}
	if rec.Status != "failed" {
		t.Errorf("a verify-faulted task must be finalized 'failed'; got %q", rec.Status)
	}
}
