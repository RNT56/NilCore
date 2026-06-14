package agent_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/verify"
)

type fakeBackend struct {
	name   string
	ran    bool
	gotDir string
}

func (f *fakeBackend) Name() string { return f.name }

func (f *fakeBackend) Run(_ context.Context, t backend.Task) (backend.Result, error) {
	f.ran = true
	f.gotDir = t.Dir
	return backend.Result{Backend: f.name, Summary: "did work", SelfClaimed: true}, nil
}

type fakeVerifier struct {
	passed  bool
	checked bool
}

func (v *fakeVerifier) Check(context.Context) (verify.Report, error) {
	v.checked = true
	return verify.Report{Passed: v.passed, Output: "checked"}, nil
}

func initGitRepo(t *testing.T) string {
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

func TestExecuteRunsInWorktreeAndVerifies(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	fb := &fakeBackend{name: "fake"}
	fv := &fakeVerifier{passed: true}

	var envDir string
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		NewEnv: func(dir string) agent.Env {
			envDir = dir
			return agent.Env{Backend: fb, Verifier: fv}
		},
		Router:  agent.SingleRouter{},
		Spawner: agent.NoSpawner{},
	}

	out, err := orch.Execute(context.Background(), backend.Task{ID: "P1-T02", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !fb.ran {
		t.Error("backend did not run")
	}
	if !fv.checked {
		t.Error("verifier was not consulted")
	}
	if !out.Verified {
		t.Error("expected Verified true")
	}
	if fb.gotDir == "" || fb.gotDir != envDir {
		t.Errorf("backend ran in %q, env built for %q (want the worktree path)", fb.gotDir, envDir)
	}
	if fb.gotDir == repo {
		t.Error("backend ran in the base repo, not an isolated worktree")
	}
	if _, err := os.Stat(envDir); !os.IsNotExist(err) {
		t.Errorf("worktree %q was not cleaned up", envDir)
	}
}

// The verifier, not the backend's self-report, decides whether work ships (I2).
func TestVerifierOverridesSelfClaim(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	fb := &fakeBackend{name: "fake"}   // returns SelfClaimed: true
	fv := &fakeVerifier{passed: false} // but the checks fail

	// Router/Spawner left nil to exercise the orchestrator's defaults.
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		NewEnv:   func(dir string) agent.Env { return agent.Env{Backend: fb, Verifier: fv} },
	}

	out, err := orch.Execute(context.Background(), backend.Task{ID: "P1-T02b", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Verified {
		t.Error("verifier failed but Outcome.Verified is true — self-claim must not decide (I2)")
	}
}
