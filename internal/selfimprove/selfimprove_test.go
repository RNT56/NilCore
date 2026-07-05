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

// TestChangedScreenBlocksOutOfScopeWrite is the finding's core guarantee: the
// scope check passes on the DECLARED paths, the run reports success, but the run
// actually WROTE a denied file (the verifier of record) — the execution-time
// Changed screen must catch it and refuse to gate, so the edit never merges.
func TestChangedScreenBlocksOutOfScopeWrite(t *testing.T) {
	gated := false
	f := &Flow{
		Scope: DefaultScope(),
		Run:   func(context.Context, string) (bool, error) { return true, nil },
		// The run touched the verifier of record despite declaring only a tool path.
		Changed: func(context.Context) ([]string, error) {
			return []string{"internal/tools/x.go", "internal/verify/verify.go"}, nil
		},
		Gate: func(string) bool { gated = true; return true },
	}
	merged, err := f.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"})
	if merged || err == nil {
		t.Fatalf("a run that wrote a denied file must be rejected: merged=%v err=%v", merged, err)
	}
	if gated {
		t.Error("the human gate must never be reached for an out-of-scope run")
	}
}

// TestChangedScreenBlocksUndeclaredWrite: even an ALLOW-listed file that the
// proposal did not declare is rejected — the run may write only what it committed
// to up front.
func TestChangedScreenBlocksUndeclaredWrite(t *testing.T) {
	f := &Flow{
		Scope:   DefaultScope(),
		Run:     func(context.Context, string) (bool, error) { return true, nil },
		Changed: func(context.Context) ([]string, error) { return []string{"internal/tools/other.go"}, nil },
		Gate:    func(string) bool { return true },
	}
	merged, err := f.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"})
	if merged || err == nil {
		t.Fatalf("an undeclared (even if allow-listed) write must be rejected: merged=%v err=%v", merged, err)
	}
}

// TestChangedScreenAllowsInScopeWrite: a run that modified exactly the declared,
// in-scope file passes the screen and merges.
func TestChangedScreenAllowsInScopeWrite(t *testing.T) {
	f := &Flow{
		Scope:   DefaultScope(),
		Run:     func(context.Context, string) (bool, error) { return true, nil },
		Changed: func(context.Context) ([]string, error) { return []string{"internal/tools/x.go"}, nil },
		Gate:    func(string) bool { return true },
	}
	merged, err := f.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"})
	if err != nil || !merged {
		t.Fatalf("an in-scope declared write must merge: merged=%v err=%v", merged, err)
	}
}

// TestChangedScreenFailsClosedOnError: if the run's footprint cannot be determined,
// Propose refuses to gate (fail-closed).
func TestChangedScreenFailsClosedOnError(t *testing.T) {
	gated := false
	f := &Flow{
		Scope:   DefaultScope(),
		Run:     func(context.Context, string) (bool, error) { return true, nil },
		Changed: func(context.Context) ([]string, error) { return nil, context.DeadlineExceeded },
		Gate:    func(string) bool { gated = true; return true },
	}
	merged, err := f.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"})
	if merged || err == nil {
		t.Fatalf("an indeterminate footprint must fail closed: merged=%v err=%v", merged, err)
	}
	if gated {
		t.Error("the gate must never be reached when the scope check is indeterminate")
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
