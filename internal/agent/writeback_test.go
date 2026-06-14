package agent_test

import (
	"context"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
)

func TestOnSuccessWritesBackOnlyWhenVerified(t *testing.T) {
	repo := initGitRepo(t)

	var gotGoal string
	pass := &agent.Orchestrator{
		BaseRepo: repo,
		NewEnv: func(string) agent.Env {
			return agent.Env{Backend: &fakeBackend{name: "f"}, Verifier: &fakeVerifier{passed: true}}
		},
		OnSuccess: func(_ context.Context, tk backend.Task, _ agent.Outcome) { gotGoal = tk.Goal },
	}
	if _, err := pass.Execute(context.Background(), backend.Task{ID: "ok", Goal: "the goal"}); err != nil {
		t.Fatal(err)
	}
	if gotGoal != "the goal" {
		t.Errorf("OnSuccess should fire on a verified task; gotGoal=%q", gotGoal)
	}

	var firedOnFail bool
	fail := &agent.Orchestrator{
		BaseRepo: repo,
		NewEnv: func(string) agent.Env {
			return agent.Env{Backend: &fakeBackend{name: "f"}, Verifier: &fakeVerifier{passed: false}}
		},
		OnSuccess: func(context.Context, backend.Task, agent.Outcome) { firedOnFail = true },
	}
	if _, err := fail.Execute(context.Background(), backend.Task{ID: "bad", Goal: "x"}); err != nil {
		t.Fatal(err)
	}
	if firedOnFail {
		t.Error("OnSuccess must not fire when the verifier failed")
	}
}
