package selfimprove

import (
	"context"
	"errors"
	"testing"
)

// verifiedRun is a Run that greens and leaves `branch` behind.
func verifiedRun(branch string) func(context.Context, string) (bool, string, error) {
	return func(context.Context, string) (bool, string, error) { return true, branch, nil }
}

// recMerge records whether the merge seam actually fired, and for which branch.
type recMerge struct {
	called bool
	branch string
	err    error
}

func (m *recMerge) fn(_ context.Context, branch string) error {
	m.called = true
	m.branch = branch
	return m.err
}

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
	m := &recMerge{}
	f := &Flow{
		Scope: DefaultScope(),
		Run: func(context.Context, string) (bool, string, error) {
			ran = true
			return true, "task/self-1", nil
		},
		Merge: m.fn,
		Gate:  func(string) bool { return true },
	}
	merged, err := f.Propose(context.Background(), Proposal{Reason: "missing tool", Paths: []string{"internal/tools/new.go"}, Goal: "add a tool"})
	if err != nil || !merged {
		t.Fatalf("in-scope verified+approved edit should merge: merged=%v err=%v", merged, err)
	}
	if !ran {
		t.Error("the edit should have run as a task")
	}
	// The whole point: merged=true must mean a merge ACTUALLY happened.
	if !m.called {
		t.Fatal("Propose reported merged=true but never called the merge seam")
	}
	if m.branch != "task/self-1" {
		t.Errorf("merged branch = %q, want the verified branch the run kept", m.branch)
	}
}

// THE regression: an approved edit with no merge wired must NOT claim to have shipped.
// Previously Propose appended `self_edit_merged` and returned true while nothing merged.
func TestApprovedWithNoMergeSeamDoesNotClaimMerge(t *testing.T) {
	f := &Flow{
		Scope: DefaultScope(),
		Run:   verifiedRun("task/self-1"),
		Gate:  func(string) bool { return true },
		// Merge deliberately nil — the pre-fix flow "merged" here.
	}
	merged, err := f.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"})
	if merged {
		t.Fatal("Propose claimed a merge with no merge seam wired — the edit never landed")
	}
	if err == nil {
		t.Fatal("an approved-but-unlandable edit must surface an error, not a silent false")
	}
}

// An approved edit whose run kept no branch has nothing to land.
func TestApprovedWithNoBranchDoesNotClaimMerge(t *testing.T) {
	m := &recMerge{}
	f := &Flow{
		Scope: DefaultScope(),
		Run:   func(context.Context, string) (bool, string, error) { return true, "", nil },
		Merge: m.fn,
		Gate:  func(string) bool { return true },
	}
	merged, err := f.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"})
	if merged || err == nil {
		t.Fatalf("no kept branch must not merge: merged=%v err=%v", merged, err)
	}
	if m.called {
		t.Error("the merge seam must not be called with an empty branch")
	}
}

// A merge that fails (e.g. a conflict) is not a merge.
func TestMergeFailureIsNotAMerge(t *testing.T) {
	m := &recMerge{err: errors.New("conflicted; tree restored")}
	f := &Flow{
		Scope: DefaultScope(),
		Run:   verifiedRun("task/self-1"),
		Merge: m.fn,
		Gate:  func(string) bool { return true },
	}
	merged, err := f.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"})
	if merged {
		t.Fatal("a failed merge must not report merged=true")
	}
	if err == nil {
		t.Fatal("a failed merge must surface its error")
	}
	if !m.called {
		t.Error("the merge seam should have been attempted")
	}
}

func TestOutOfScopeEditRejectedBeforeRunning(t *testing.T) {
	f := &Flow{
		Scope: DefaultScope(),
		Run: func(context.Context, string) (bool, string, error) {
			t.Fatal("must not run an out-of-scope edit")
			return false, "", nil
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
	m := &recMerge{}
	f := &Flow{
		Scope: DefaultScope(),
		Run:   verifiedRun("task/self-1"),
		// The run touched the verifier of record despite declaring only a tool path.
		Changed: func(context.Context) ([]string, error) {
			return []string{"internal/tools/x.go", "internal/verify/verify.go"}, nil
		},
		Merge: m.fn,
		Gate:  func(string) bool { gated = true; return true },
	}
	merged, err := f.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"})
	if merged || err == nil {
		t.Fatalf("a run that wrote a denied file must be rejected: merged=%v err=%v", merged, err)
	}
	if gated {
		t.Error("the human gate must never be reached for an out-of-scope run")
	}
	if m.called {
		t.Error("the verifier of record must never be merged")
	}
}

// TestChangedScreenBlocksUndeclaredWrite: even an ALLOW-listed file that the
// proposal did not declare is rejected — the run may write only what it committed
// to up front.
func TestChangedScreenBlocksUndeclaredWrite(t *testing.T) {
	f := &Flow{
		Scope:   DefaultScope(),
		Run:     verifiedRun("task/self-1"),
		Changed: func(context.Context) ([]string, error) { return []string{"internal/tools/other.go"}, nil },
		Merge:   (&recMerge{}).fn,
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
	m := &recMerge{}
	f := &Flow{
		Scope:   DefaultScope(),
		Run:     verifiedRun("task/self-1"),
		Changed: func(context.Context) ([]string, error) { return []string{"internal/tools/x.go"}, nil },
		Merge:   m.fn,
		Gate:    func(string) bool { return true },
	}
	merged, err := f.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"})
	if err != nil || !merged {
		t.Fatalf("an in-scope declared write must merge: merged=%v err=%v", merged, err)
	}
	if !m.called {
		t.Error("merged=true must mean the merge seam ran")
	}
}

// TestChangedScreenFailsClosedOnError: if the run's footprint cannot be determined,
// Propose refuses to gate (fail-closed).
func TestChangedScreenFailsClosedOnError(t *testing.T) {
	gated := false
	f := &Flow{
		Scope:   DefaultScope(),
		Run:     verifiedRun("task/self-1"),
		Changed: func(context.Context) ([]string, error) { return nil, context.DeadlineExceeded },
		Merge:   (&recMerge{}).fn,
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
	// Verified but gate denies → no merge, and the merge seam never fires.
	dm := &recMerge{}
	denied := &Flow{Scope: DefaultScope(), Run: verifiedRun("task/self-1"), Merge: dm.fn, Gate: func(string) bool { return false }}
	if merged, _ := denied.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"}); merged {
		t.Error("a denied gate must block the merge")
	}
	if dm.called {
		t.Error("a denied gate must never reach the merge")
	}

	// Unverified → no merge, even if the gate would approve.
	um := &recMerge{}
	unver := &Flow{
		Scope: DefaultScope(),
		Run:   func(context.Context, string) (bool, string, error) { return false, "task/self-1", nil },
		Merge: um.fn,
		Gate:  func(string) bool { return true },
	}
	if merged, _ := unver.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"}); merged {
		t.Error("an unverified edit must not merge")
	}
	if um.called {
		t.Error("an unverified edit must never reach the merge")
	}
}
