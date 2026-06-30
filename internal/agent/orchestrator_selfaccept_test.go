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

// stubApprover is a policy.Approver test double: it records that it was asked and
// returns a fixed verdict.
type stubApprover struct {
	ok    bool
	asked bool
}

func (s *stubApprover) Approve(string) bool {
	s.asked = true
	return s.ok
}

// recordingSelfAccept captures whether the closed-loop hook was consulted and what it
// received (goal, box, and the result of calling the threaded gate), and returns a
// configurable verdict.
type recordingSelfAccept struct {
	called      bool
	gotGoal     string
	gotBox      sandbox.Sandbox
	gateAllowed bool
	passed      bool
	detail      string
}

func (r *recordingSelfAccept) fn() agent.SelfAcceptFunc {
	return func(_ context.Context, goal string, box sandbox.Sandbox, gate func(policy.GateAction) bool, _ *eventlog.Log) (bool, string) {
		r.called = true
		r.gotGoal = goal
		r.gotBox = box
		r.gateAllowed = gate(policy.GateAction{Type: policy.BindSelfAuthored, Branch: "candidate.x"})
		return r.passed, r.detail
	}
}

// sentinelBox is an identity-comparable sandbox.Sandbox so a test can assert the hook
// received the very Env.Box the factory built.
type sentinelBox struct{}

func (sentinelBox) Exec(context.Context, string) (sandbox.Result, error) {
	return sandbox.Result{}, nil
}
func (sentinelBox) ExecWithEnv(context.Context, string, map[string]string) (sandbox.Result, error) {
	return sandbox.Result{}, nil
}
func (sentinelBox) Workdir() string { return "" }

// TestSelfAcceptReceivesBoxAndWorkingGate: the orchestrator threads env.Box and a gate
// that routes to o.Approver into the hook — an approving approver ⇒ gate allows; a nil
// approver ⇒ gate deny-defaults (so a headless run can never auto-bind).
func TestSelfAcceptReceivesBoxAndWorkingGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	box := sentinelBox{}
	newEnv := func(dir string) agent.Env {
		return agent.Env{Backend: &fakeBackend{name: "fake"}, Verifier: &fakeVerifier{passed: true}, Box: box}
	}

	sa := &recordingSelfAccept{passed: true}
	orch := &agent.Orchestrator{BaseRepo: repo, NewEnv: newEnv, Approver: &stubApprover{ok: true}, SelfAccept: sa.fn()}
	if _, err := orch.Execute(context.Background(), backend.Task{ID: "sa-box", Goal: "g"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if sa.gotBox != box {
		t.Error("hook did not receive env.Box")
	}
	if !sa.gateAllowed {
		t.Error("gate did not route to the approving Approver")
	}

	saNil := &recordingSelfAccept{passed: true}
	orchNil := &agent.Orchestrator{BaseRepo: repo, NewEnv: newEnv, Approver: nil, SelfAccept: saNil.fn()}
	if _, err := orchNil.Execute(context.Background(), backend.Task{ID: "sa-box2", Goal: "g"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if saNil.gateAllowed {
		t.Error("a nil Approver must deny-default the threaded gate")
	}
}

// TestSelfAcceptReddenSkipsOnSuccessAndKeepBranch: when self-acceptance reddens a green
// floor, the success side-effects must NOT fire — OnSuccess is not called and no branch
// is preserved (the run is not "done").
func TestSelfAcceptReddenSkipsOnSuccessAndKeepBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitRepo(t)
	onSuccess := false
	sa := &recordingSelfAccept{passed: false, detail: "candidate.x failed"}
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		NewEnv: func(dir string) agent.Env {
			return agent.Env{Backend: &fakeBackend{name: "fake"}, Verifier: &fakeVerifier{passed: true}, Box: sentinelBox{}}
		},
		SelfAccept: sa.fn(),
		KeepBranch: true,
		OnSuccess:  func(context.Context, backend.Task, agent.Outcome) { onSuccess = true },
	}
	out, err := orch.Execute(context.Background(), backend.Task{ID: "sa-redden", Goal: "g"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Verified {
		t.Error("self-acceptance failure must redden the verdict")
	}
	if onSuccess {
		t.Error("OnSuccess must NOT fire when self-acceptance reddens the verdict")
	}
	if out.Branch != "" {
		t.Error("KeepBranch must not preserve a branch on a self-acceptance-reddened verdict")
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
