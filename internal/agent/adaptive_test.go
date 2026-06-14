package agent_test

import (
	"context"
	"sync/atomic"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/planner"
	"nilcore/internal/spawn"
)

func twoTaskPlan() func(context.Context, string) (planner.Tree, error) {
	return func(context.Context, string) (planner.Tree, error) {
		return planner.Tree{Tasks: []planner.PlanTask{
			{ID: "t1", Goal: "write test", Acceptance: "fails first"},
			{ID: "t2", Goal: "implement", Acceptance: "passes"},
		}}, nil
	}
}

func TestExecutePlannedDecomposesAndAggregates(t *testing.T) {
	var ran int32
	orch := &agent.Orchestrator{
		Plan:        twoTaskPlan(),
		ShouldPlan:  func(string) bool { return true },
		MaxParallel: 2,
		RunSub: func(_ context.Context, st spawn.Subtask) spawn.Result {
			atomic.AddInt32(&ran, 1)
			return spawn.Result{ID: st.ID, Summary: st.Goal, Passed: true}
		},
	}
	out, err := orch.Execute(context.Background(), backend.Task{ID: "top", Goal: "a complex goal"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Verified {
		t.Error("all subtasks passed; aggregate should be verified")
	}
	if ran != 2 {
		t.Errorf("ran %d subtasks, want 2", ran)
	}
}

func TestExecutePlannedFailingSubtask(t *testing.T) {
	orch := &agent.Orchestrator{
		Plan:       twoTaskPlan(),
		ShouldPlan: func(string) bool { return true },
		RunSub: func(_ context.Context, st spawn.Subtask) spawn.Result {
			return spawn.Result{ID: st.ID, Passed: st.ID == "t1"} // t2 fails
		},
	}
	out, err := orch.Execute(context.Background(), backend.Task{ID: "top", Goal: "complex"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Verified {
		t.Error("a failing subtask must make the aggregate not verified")
	}
}

// When planning declines (ShouldPlan false), Execute uses the single-task path —
// even with a Plan configured.
func TestPlanDeclinedFallsBackToSingle(t *testing.T) {
	repo := initGitRepo(t)
	fb := &fakeBackend{name: "single"}
	fv := &fakeVerifier{passed: true}
	orch := &agent.Orchestrator{
		BaseRepo:   repo,
		NewEnv:     func(string) agent.Env { return agent.Env{Backend: fb, Verifier: fv} },
		Plan:       twoTaskPlan(),
		ShouldPlan: func(string) bool { return false }, // decline → single path
		RunSub:     func(context.Context, spawn.Subtask) spawn.Result { panic("must not run subtasks") },
	}
	out, err := orch.Execute(context.Background(), backend.Task{ID: "P3-T05", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Backend != "single" || !out.Verified {
		t.Errorf("expected single-task path; got %+v", out)
	}
}
