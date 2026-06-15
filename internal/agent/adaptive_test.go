package agent_test

import (
	"context"
	"path/filepath"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/project"
	"nilcore/internal/verify"
)

// tmpLog opens a throwaway append-only log under the test's temp dir.
func tmpLog(t *testing.T) *eventlog.Log {
	t.Helper()
	lg, err := eventlog.Open(filepath.Join(t.TempDir(), "events.log"))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	return lg
}

// convergingLoop builds a minimal, hermetic project.Loop (no network, no real
// git/sandbox) that converges to Done in one pass: a passing verifier plus a
// single seeded criterion so done-detection runs at iteration 0.
func convergingLoop(t *testing.T) *project.Loop {
	t.Helper()
	pass := func(string) verify.Verifier { return &fakeVerifier{passed: true} }
	l := &project.Loop{
		Goal: "a complex multi-slice goal",
		Repo: t.TempDir(),
		Log:  tmpLog(t),
		Plan: func(context.Context, string, project.State) (project.Slice, error) {
			return project.Slice{Goal: "slice"}, nil
		},
		RunSlice: func(context.Context, project.Slice, project.State) (project.SliceResult, error) {
			return project.SliceResult{Verified: true}, nil
		},
		Verifier: pass,
	}
	l.SeedCriteria([]project.Criterion{{Command: "true", Verifier: &fakeVerifier{passed: true}}})
	return l
}

// When the supervision seam is wired and ShouldSupervise judges the goal complex,
// Execute hands the goal to the project loop and folds its verifier verdict back —
// the single-task backend never runs.
func TestExecuteRoutesToProjectLoop(t *testing.T) {
	fb := &fakeBackend{name: "single"}
	orch := &agent.Orchestrator{
		Log:             tmpLog(t),
		NewEnv:          func(string) agent.Env { return agent.Env{Backend: fb, Verifier: &fakeVerifier{passed: true}} },
		Project:         convergingLoop(t),
		ShouldSupervise: func(string) bool { return true },
	}
	out, err := orch.Execute(context.Background(), backend.Task{ID: "top", Goal: "a complex goal"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Backend != "project" {
		t.Errorf("Backend = %q, want project (the supervised path)", out.Backend)
	}
	if !out.Verified {
		t.Error("the project loop converged; aggregate should be verified")
	}
	if fb.ran {
		t.Error("the single-task backend must not run on the supervised path")
	}
}

// ShouldSupervise judging the goal simple keeps the single-task path even with a
// Project wired — the project loop never runs.
func TestSuperviseDeclinedFallsBackToSingle(t *testing.T) {
	repo := initGitRepo(t)
	fb := &fakeBackend{name: "single"}
	fv := &fakeVerifier{passed: true}
	orch := &agent.Orchestrator{
		Log:             tmpLog(t),
		BaseRepo:        repo,
		NewEnv:          func(string) agent.Env { return agent.Env{Backend: fb, Verifier: fv} },
		Project:         convergingLoop(t),
		ShouldSupervise: func(string) bool { return false }, // decline → single path
	}
	out, err := orch.Execute(context.Background(), backend.Task{ID: "P5-T01", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Backend != "single" || !out.Verified {
		t.Errorf("expected single-task path; got %+v", out)
	}
	if !fb.ran {
		t.Error("the single-task backend must run when supervision is declined")
	}
}

// With Project==nil the supervision seam is inert: Execute is the single-task path
// even when ShouldSupervise is wired (and would return true).
func TestNilProjectKeepsSingleTaskPath(t *testing.T) {
	repo := initGitRepo(t)
	fb := &fakeBackend{name: "single"}
	fv := &fakeVerifier{passed: true}
	orch := &agent.Orchestrator{
		Log:             tmpLog(t),
		BaseRepo:        repo,
		NewEnv:          func(string) agent.Env { return agent.Env{Backend: fb, Verifier: fv} },
		ShouldSupervise: func(string) bool { return true }, // wired, but Project==nil
	}
	out, err := orch.Execute(context.Background(), backend.Task{ID: "P5-T01", Goal: "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Backend != "single" || !out.Verified {
		t.Errorf("Project==nil must keep the single-task path; got %+v", out)
	}
}
