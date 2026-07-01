package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/graapprove"
	"nilcore/internal/onboard"
	"nilcore/internal/policy"
)

// TestWrapAutoApprove_KillSwitchRootWired proves wrapAutoApprove resolves the kill-switch
// sentinel against the RUN ROOT it is given (not the process CWD): the same envelope +
// action auto-approves on a clean root but is DISABLED when the operator drops
// .nilcore/AUTOAPPROVE_OFF into that root. Regression for the "WithRoot never wired" gap.
func TestWrapAutoApprove_KillSwitchRootWired(t *testing.T) {
	env, err := graapprove.Preset("conservative")
	if err != nil {
		t.Fatalf("preset: %v", err)
	}
	cfg := onboard.Config{AutoApprove: &env}
	action := policy.GateAction{Type: policy.OpenPR, Branch: "feature-x"}

	// Earn trust for the open-pr boundary: the conservative preset needs MinSuccesses/
	// MinSample=5. Seed 5 verifier-green open-pr outcomes (scopeFor == Branch) into a log.
	logPath := filepath.Join(t.TempDir(), "e.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	for i := 0; i < 5; i++ {
		emitBoundaryOutcome(log, policy.OpenPR.String(), "feature-x", true)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}

	structured := func(a policy.Approver) interface {
		ApproveStructured(policy.GateAction) bool
	} {
		gs, ok := a.(interface {
			ApproveStructured(policy.GateAction) bool
		})
		if !ok {
			t.Fatalf("wrapped approver must expose ApproveStructured, got %T", a)
		}
		return gs
	}

	// Clean root + earned trust: the conservative envelope auto-approves open-pr (the human
	// deny is never consulted because auto-approval fired).
	cleanRoot := t.TempDir()
	if !structured(wrapAutoApprove(denyAllApprover{}, cfg, cleanRoot, logPath, nil, nil)).ApproveStructured(action) {
		t.Fatal("conservative envelope must auto-approve open-pr on a clean root with earned trust")
	}

	// Same root with the kill-switch sentinel dropped in: auto-approval is DISABLED and the
	// action falls through to the (denying) human — proving the sentinel resolved against
	// the RUN ROOT we passed, not the process CWD.
	offRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(offRoot, ".nilcore"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(offRoot, ".nilcore", "AUTOAPPROVE_OFF"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if structured(wrapAutoApprove(denyAllApprover{}, cfg, offRoot, logPath, nil, nil)).ApproveStructured(action) {
		t.Fatal("a kill-switch sentinel at the run root must disable auto-approval (WithRoot not wired?)")
	}
}

func TestWrapAutoApprove_DefaultOffByteIdentical(t *testing.T) {
	human := policy.NewConsoleApprover(strings.NewReader(""), io.Discard)
	// No envelope ⇒ the human approver is returned UNCHANGED (same value).
	if got := wrapAutoApprove(human, onboard.Config{}, "", "x.jsonl", nil, nil); got != policy.Approver(human) {
		t.Errorf("no envelope must return the human approver unchanged (byte-identical default-off)")
	}
	// A nil human (e.g. the swarm approver) is returned as-is — never wrapped.
	if got := wrapAutoApprove(nil, onboard.Config{}, "", "x.jsonl", nil, nil); got != nil {
		t.Errorf("nil human must be returned as-is, got %v", got)
	}
}

func TestWrapAutoApprove_WrapsWhenConfigured(t *testing.T) {
	env, err := graapprove.Preset("conservative")
	if err != nil {
		t.Fatalf("preset: %v", err)
	}
	human := policy.NewConsoleApprover(strings.NewReader("y\n"), io.Discard)
	got := wrapAutoApprove(human, onboard.Config{AutoApprove: &env}, "", "x.jsonl", nil, nil)
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
	emitBoundaryOutcome(log, policy.PromoteToBase.String(), "feature-x", true)
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
	got := view.Tally(graapprove.ScopeKey{Type: policy.PromoteToBase.String(), Scope: "feature-x"})
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
	autoApproveSink{log}.Emit("auto_approve", map[string]any{"action": "open-pr", "scope": "feature-x"})
	if err := log.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A well-formed event was appended without breaking the hash chain.
	if err := eventlog.Verify(path); err != nil {
		t.Fatalf("sink Emit broke the event-log chain: %v", err)
	}
}
