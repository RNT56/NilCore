package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/graapprove"
	"nilcore/internal/onboard"
	"nilcore/internal/policy"
)

func TestWrapAutoApprove_DefaultOffByteIdentical(t *testing.T) {
	human := policy.NewConsoleApprover(strings.NewReader(""), io.Discard)
	// No envelope ⇒ the human approver is returned UNCHANGED (same value).
	if got := wrapAutoApprove(human, onboard.Config{}, "x.jsonl", nil, nil); got != policy.Approver(human) {
		t.Errorf("no envelope must return the human approver unchanged (byte-identical default-off)")
	}
	// A nil human (e.g. the swarm approver) is returned as-is — never wrapped.
	if got := wrapAutoApprove(nil, onboard.Config{}, "x.jsonl", nil, nil); got != nil {
		t.Errorf("nil human must be returned as-is, got %v", got)
	}
}

func TestWrapAutoApprove_WrapsWhenConfigured(t *testing.T) {
	env, err := graapprove.Preset("conservative")
	if err != nil {
		t.Fatalf("preset: %v", err)
	}
	human := policy.NewConsoleApprover(strings.NewReader("y\n"), io.Discard)
	got := wrapAutoApprove(human, onboard.Config{AutoApprove: &env}, "x.jsonl", nil, nil)
	if got == policy.Approver(human) {
		t.Errorf("a configured envelope must yield a wrapped (graded) approver, not the bare human")
	}
}

func TestEmitBoundaryOutcome_FoldsToEarnedTrust(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	// A verifier-green promote boundary (the shape swarm.run / project.converge emit).
	emitBoundaryOutcome(log, policy.PromoteToBase.String(), "feature/x", true)
	if err := log.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// The event must keep the hash chain intact (I5)...
	if err := eventlog.Verify(path); err != nil {
		t.Fatalf("emitBoundaryOutcome broke the event-log chain: %v", err)
	}
	// ...and fold into earned trust as one verifier-green sample (the GAA-T04 → GAA-T05
	// hand-off that makes graduated auto-approval able to fire).
	view, err := graapprove.BuildTrust(path)
	if err != nil {
		t.Fatalf("BuildTrust: %v", err)
	}
	if !view.ChainOK {
		t.Fatal("a clean chain must report ChainOK=true")
	}
	got := view.Tally(graapprove.ScopeKey{Type: policy.PromoteToBase.String(), Scope: "feature/x"})
	if got.Green != 1 || got.Total != 1 {
		t.Fatalf("boundary_outcome must fold to 1 green / 1 total, got %+v", got)
	}
	// A self-report (passed=false) must NOT count as a green (I2: only verifier verdicts).
	other := view.Tally(graapprove.ScopeKey{Type: policy.PromoteToBase.String(), Scope: "feature/never"})
	if other.Green != 0 || other.Total != 0 {
		t.Fatalf("an un-emitted scope must be empty, got %+v", other)
	}
}

func TestAutoApproveSink_AppendsToChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	autoApproveSink{log}.Emit("auto_approve", map[string]any{"action": "open-pr", "scope": "feature/x"})
	if err := log.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A well-formed event was appended without breaking the hash chain.
	if err := eventlog.Verify(path); err != nil {
		t.Fatalf("sink Emit broke the event-log chain: %v", err)
	}
}
