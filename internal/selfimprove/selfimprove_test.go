package selfimprove

import (
	"context"
	"testing"
)

func TestScopeCheck(t *testing.T) {
	s := DefaultScope()
	if ok, _ := s.Check(Proposal{Paths: []string{"internal/skills/greet.go"}}); !ok {
		t.Error("editing a skill should be in scope")
	}
	if ok, reason := s.Check(Proposal{Paths: []string{"internal/agent/orchestrator.go"}}); ok {
		t.Errorf("editing the core must be rejected; reason=%q", reason)
	}
	if ok, _ := s.Check(Proposal{Paths: []string{"go.mod"}}); ok {
		t.Error("editing a contract file must be rejected")
	}
	if ok, _ := s.Check(Proposal{Paths: []string{"internal/random/thing.go"}}); ok {
		t.Error("editing outside the allow-list must be rejected")
	}
}

func TestInScopeEditGatedAndMerges(t *testing.T) {
	var ran bool
	f := &Flow{
		Scope: DefaultScope(),
		Run:   func(context.Context, string) (bool, error) { ran = true; return true, nil },
		Gate:  func(string) bool { return true },
	}
	merged, err := f.Propose(context.Background(), Proposal{Reason: "missing tool", Paths: []string{"internal/tools/new.go"}, Goal: "add a tool"})
	if err != nil || !merged {
		t.Fatalf("in-scope verified+approved edit should merge: merged=%v err=%v", merged, err)
	}
	if !ran {
		t.Error("the edit should have run as a task")
	}
}

func TestOutOfScopeEditRejectedBeforeRunning(t *testing.T) {
	f := &Flow{
		Scope: DefaultScope(),
		Run: func(context.Context, string) (bool, error) {
			t.Fatal("must not run an out-of-scope edit")
			return false, nil
		},
		Gate: func(string) bool { return true },
	}
	merged, err := f.Propose(context.Background(), Proposal{Paths: []string{"internal/sandbox/sandbox.go"}, Goal: "weaken the sandbox"})
	if err == nil || merged {
		t.Error("an edit touching the core must be rejected by the scope check")
	}
}

func TestGateAndVerifierBlockMerge(t *testing.T) {
	// Verified but gate denies → no merge.
	denied := &Flow{Scope: DefaultScope(), Run: func(context.Context, string) (bool, error) { return true, nil }, Gate: func(string) bool { return false }}
	if merged, _ := denied.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"}); merged {
		t.Error("a denied gate must block the merge")
	}
	// Unverified → no merge, even if the gate would approve.
	unver := &Flow{Scope: DefaultScope(), Run: func(context.Context, string) (bool, error) { return false, nil }, Gate: func(string) bool { return true }}
	if merged, _ := unver.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"}); merged {
		t.Error("an unverified edit must not merge")
	}
}
