package backend

import (
	"context"
	"testing"

	"nilcore/internal/advisor"
	"nilcore/internal/model"
)

// RTE-T05: EscalateAfterFn supplies the auto-escalation threshold dynamically in
// place of the static EscalateAfter field. The caller wires it from
// agent.OracleEscalateAfter so the trust oracle can size the budget per task-class.
// These tests prove (1) the dynamic budget is honored over the static field, and (2)
// a nil EscalateAfterFn falls back to the static field exactly — byte-identical to
// the pre-RTE escalation gate.

// twoFinishModel scripts two finish attempts: the first fails verification (forcing
// an escalation decision), the second passes — the same shape the existing advisor
// auto-escalation test uses.
func twoFinishModel() *scriptModel {
	return &scriptModel{responses: []model.Response{
		{Content: []model.Block{toolUse("u1", "finish", map[string]string{"summary": "try 1"})}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u2", "finish", map[string]string{"summary": "try 2"})}, StopReason: "tool_use"},
	}}
}

// TestNativeEscalateAfterFnHonored: with the static EscalateAfter at 0 (off) but a
// dynamic EscalateAfterFn returning 1, the loop auto-consults the advisor after the
// first verifier failure — proving the fn, not the static field, sets the budget.
func TestNativeEscalateAfterFnHonored(t *testing.T) {
	adv := advisor.New(adviceModel("check the imports"), 4)
	n := &Native{
		Model:    twoFinishModel(),
		Box:      &recordingBox{},
		Verifier: &flakyVerifier{failFirst: 1},
		Advisor:  adv,
		// Static field OFF; the dynamic budget alone turns escalation on.
		EscalateAfter:   0,
		EscalateAfterFn: func() int { return 1 },
		MaxSteps:        5,
	}

	res, err := n.Run(context.Background(), Task{ID: "fn-on", Goal: "x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if adv.Calls() < 1 {
		t.Error("dynamic EscalateAfterFn=1 should auto-escalate after a verifier failure")
	}
	if !res.SelfClaimed {
		t.Error("loop should pass on the second attempt")
	}
}

// TestNativeEscalateAfterFnSuppresses: a dynamic budget HIGHER than the failure run
// suppresses an escalation the static field would have triggered — proving the fn
// fully replaces the static threshold (the oracle may widen a proven class's budget).
func TestNativeEscalateAfterFnSuppresses(t *testing.T) {
	adv := advisor.New(adviceModel("check the imports"), 4)
	n := &Native{
		Model:    twoFinishModel(),
		Box:      &recordingBox{},
		Verifier: &flakyVerifier{failFirst: 1},
		Advisor:  adv,
		// Static field WOULD escalate after 1 failure, but the dynamic budget (99)
		// raises the bar above the single failure this run produces.
		EscalateAfter:   1,
		EscalateAfterFn: func() int { return 99 },
		MaxSteps:        5,
	}

	res, err := n.Run(context.Background(), Task{ID: "fn-high", Goal: "x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if adv.Calls() != 0 {
		t.Errorf("dynamic EscalateAfterFn=99 should suppress escalation, got %d advisor calls", adv.Calls())
	}
	if !res.SelfClaimed {
		t.Error("loop should still pass on the second attempt")
	}
}

// TestNativeEscalateAfterNilFnUsesStatic is the default-off proof: with
// EscalateAfterFn nil the static EscalateAfter governs exactly as before — escalating
// after the configured number of failures, byte-identical to the pre-RTE loop.
func TestNativeEscalateAfterNilFnUsesStatic(t *testing.T) {
	adv := advisor.New(adviceModel("check the imports"), 4)
	n := &Native{
		Model:         twoFinishModel(),
		Box:           &recordingBox{},
		Verifier:      &flakyVerifier{failFirst: 1},
		Advisor:       adv,
		EscalateAfter: 1, // static threshold; no dynamic fn ⇒ static path
		MaxSteps:      5,
	}

	res, err := n.Run(context.Background(), Task{ID: "fn-nil", Goal: "x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if adv.Calls() < 1 {
		t.Error("nil EscalateAfterFn must fall back to the static EscalateAfter=1 escalation")
	}
	if !res.SelfClaimed {
		t.Error("loop should pass on the second attempt")
	}
}

// TestNativeEscalateAfterHelperDefaults asserts the helper's nil-fn fallback directly:
// escalateAfter() returns the static field verbatim when no dynamic fn is wired, and
// the fn's value when one is.
func TestNativeEscalateAfterHelperDefaults(t *testing.T) {
	if got := (&Native{EscalateAfter: 3}).escalateAfter(); got != 3 {
		t.Errorf("nil fn: escalateAfter() = %d, want static 3", got)
	}
	if got := (&Native{EscalateAfter: 3, EscalateAfterFn: func() int { return 7 }}).escalateAfter(); got != 7 {
		t.Errorf("wired fn: escalateAfter() = %d, want dynamic 7", got)
	}
}
