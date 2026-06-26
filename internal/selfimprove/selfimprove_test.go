package selfimprove

import (
	"context"
	"errors"
	"testing"
)

var errBoom = errors.New("boom")

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

// TestNilMeasureIsInert proves the additive measured-delta fence (SIF-T05) is
// wholly inert when the Measure hook is nil: a verified, in-scope, approved edit
// merges exactly as before, so the legacy propose-edit path is byte-identical.
func TestNilMeasureIsInert(t *testing.T) {
	var ran bool
	f := &Flow{
		Scope:   DefaultScope(),
		Run:     func(context.Context, string) (bool, error) { ran = true; return true, nil },
		Gate:    func(string) bool { return true },
		Measure: nil, // unwired — the fence must not change a thing
	}
	merged, err := f.Propose(context.Background(), Proposal{Reason: "missing tool", Paths: []string{"internal/tools/new.go"}, Goal: "add a tool"})
	if err != nil || !merged {
		t.Fatalf("nil Measure must leave the flow byte-identical: merged=%v err=%v", merged, err)
	}
	if !ran {
		t.Error("the edit should still have run as a task")
	}
}

// TestMeasureBlocksWhenNotImproved proves a "not improved" verdict blocks
// acceptance even though the candidate is in-scope, verifier-green, and the gate
// would approve — the fence is an additional bar, not a replacement.
func TestMeasureBlocksWhenNotImproved(t *testing.T) {
	gateAsked := false
	f := &Flow{
		Scope:   DefaultScope(),
		Run:     func(context.Context, string) (bool, error) { return true, nil },
		Gate:    func(string) bool { gateAsked = true; return true },
		Measure: func(context.Context, Proposal) (bool, error) { return false, nil },
	}
	merged, err := f.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"})
	if err != nil {
		t.Fatalf("a not-improved candidate is dropped, not an error: err=%v", err)
	}
	if merged {
		t.Error("a candidate with no measured improvement must not merge")
	}
	if gateAsked {
		t.Error("the human gate must not be reached once the measure fence rejects")
	}
}

// TestMeasurePassesWhenImproved proves an "improved" verdict lets the candidate
// proceed to the (still-mandatory) human gate and merge.
func TestMeasurePassesWhenImproved(t *testing.T) {
	measured := false
	f := &Flow{
		Scope:   DefaultScope(),
		Run:     func(context.Context, string) (bool, error) { return true, nil },
		Gate:    func(string) bool { return true },
		Measure: func(context.Context, Proposal) (bool, error) { measured = true; return true, nil },
	}
	merged, err := f.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"})
	if err != nil || !merged {
		t.Fatalf("an improved, gated candidate should merge: merged=%v err=%v", merged, err)
	}
	if !measured {
		t.Error("the measure fence should have been consulted")
	}
}

// TestMeasureErrorAborts proves a measure error aborts the proposal (wrapped),
// with no merge.
func TestMeasureErrorAborts(t *testing.T) {
	f := &Flow{
		Scope:   DefaultScope(),
		Run:     func(context.Context, string) (bool, error) { return true, nil },
		Gate:    func(string) bool { return true },
		Measure: func(context.Context, Proposal) (bool, error) { return false, errBoom },
	}
	merged, err := f.Propose(context.Background(), Proposal{Paths: []string{"internal/tools/x.go"}, Goal: "g"})
	if err == nil {
		t.Fatal("a measure error must abort the proposal")
	}
	if merged {
		t.Error("a measure error must not merge")
	}
}
