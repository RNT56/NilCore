package agent_test

import (
	"context"
	"os/exec"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/policy"
	"nilcore/internal/sandbox"
)

// recordingSelfAccept captures whether the closed-loop hook was consulted and what it
// received, and returns a configurable verdict.
type recordingSelfAccept struct {
	called  bool
	gotGoal string
	passed  bool
	detail  string
}

func (r *recordingSelfAccept) fn() agent.SelfAcceptFunc {
	return func(_ context.Context, goal string, _ sandbox.Sandbox, _ func(policy.GateAction) bool, _ *eventlog.Log) (bool, string) {
		r.called = true
		r.gotGoal = goal
		return r.passed, r.detail
	}
}

// TestSelfAcceptRedensGreenFloor: when the floor verifier passes, the hook is consulted
// with the goal, and a false result reddens the verdict (it ADDS to the bar — I2).
func TestSelfAcceptRedensGreenFloor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	fb := &fakeBackend{name: "fake"}
	fv := &fakeVerifier{passed: true} // floor GREEN
	sa := &recordingSelfAccept{passed: false, detail: "candidate.x failed"}

	orch := &agent.Orchestrator{
		BaseRepo:   repo,
		NewEnv:     func(dir string) agent.Env { return agent.Env{Backend: fb, Verifier: fv} },
		SelfAccept: sa.fn(),
	}
	out, err := orch.Execute(context.Background(), backend.Task{ID: "sa-1", Goal: "build the widget"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !sa.called {
		t.Error("hook must be consulted when the floor is green")
	}
	if sa.gotGoal != "build the widget" {
		t.Errorf("goal not threaded to the hook, got %q", sa.gotGoal)
	}
	if out.Verified {
		t.Error("a failed self-acceptance check must redden the verdict (I2: it only ADDS to the bar)")
	}
}

// TestSelfAcceptNotConsultedOnRedFloor: a red floor stays red and the hook is NEVER run
// — self-acceptance can only ever raise the bar, never rescue a red verdict.
func TestSelfAcceptNotConsultedOnRedFloor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	fb := &fakeBackend{name: "fake"}
	fv := &fakeVerifier{passed: false} // floor RED
	sa := &recordingSelfAccept{passed: true}

	orch := &agent.Orchestrator{
		BaseRepo:   repo,
		NewEnv:     func(dir string) agent.Env { return agent.Env{Backend: fb, Verifier: fv} },
		SelfAccept: sa.fn(),
	}
	out, err := orch.Execute(context.Background(), backend.Task{ID: "sa-2", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if sa.called {
		t.Error("hook must NOT run when the floor is red")
	}
	if out.Verified {
		t.Error("a red floor must stay red")
	}
}

// TestSelfAcceptPassKeepsGreen: floor green + self-acceptance green ⇒ verified.
func TestSelfAcceptPassKeepsGreen(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	fb := &fakeBackend{name: "fake"}
	fv := &fakeVerifier{passed: true}
	sa := &recordingSelfAccept{passed: true}

	orch := &agent.Orchestrator{
		BaseRepo:   repo,
		NewEnv:     func(dir string) agent.Env { return agent.Env{Backend: fb, Verifier: fv} },
		SelfAccept: sa.fn(),
	}
	out, err := orch.Execute(context.Background(), backend.Task{ID: "sa-3", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !sa.called || !out.Verified {
		t.Errorf("floor green + self-acceptance green must verify; called=%v verified=%v", sa.called, out.Verified)
	}
}

// TestSelfAcceptNilHookByteIdentical: with no hook wired, a green floor verifies exactly
// as before (the opt-in is truly off by default).
func TestSelfAcceptNilHookByteIdentical(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		NewEnv: func(dir string) agent.Env {
			return agent.Env{Backend: &fakeBackend{name: "fake"}, Verifier: &fakeVerifier{passed: true}}
		},
	}
	out, err := orch.Execute(context.Background(), backend.Task{ID: "sa-4", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Verified {
		t.Error("a nil SelfAccept hook must leave a green verdict unchanged")
	}
}
