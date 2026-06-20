package super

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"nilcore/internal/model"
	"nilcore/internal/verify"
)

// P11-T22 — the nil-gated RequeueHook consulted ONLY at convergence-red.
//
// These tests drive the REAL Run loop with a scripted model + a fake verifier and a
// fake hook, asserting the four guarantees from the spec:
//   1. nil hook  ⇒ loop byte-identical (same Outcome/round count as the baseline).
//   2. set + red ⇒ hook consulted exactly once per convergence-red, NEVER on green;
//      remaining non-exhausted ⇒ one additional focused round runs (the unit ids are
//      folded as trusted control text into the next turn).
//   3. exhausted ⇒ converge red immediately, no extra round (bounded-retry rail).
//   4. super gains no new import (compile-time: this file imports only stdlib + model
//      + verify, exactly like the other run-loop tests).

// greenAfterN passes on the Nth Check (1-indexed) and fails before it. It lets a test
// model a claim that re-derives green only after a focused requeue round, so the hook
// path can flip the run from red to converged.
type greenAfterN struct {
	n    int32 // pass once this many Checks have run
	seen int32
}

func (g *greenAfterN) Check(context.Context) (verify.Report, error) {
	c := atomic.AddInt32(&g.seen, 1)
	if c >= g.n {
		return verify.Report{Passed: true, Output: "ok"}, nil
	}
	return verify.Report{Passed: false, Output: "claim red"}, nil
}

// TestRequeueHookNilByteIdentical: with NO hook wired, a red verifier keeps the loop
// going to MaxRounds exactly as today — the requeue block is skipped wholesale.
func TestRequeueHookNilByteIdentical(t *testing.T) {
	// A model that finishes every round; the verifier always fails, so each finish is a
	// convergence-red. With no hook, the loop never short-circuits — it rides to the
	// MaxRounds ceiling, the pre-requeue baseline.
	fv := &failVerifier{}
	m := &scriptModel{responses: repeatFinish(8)}
	s := baseSup(m, fv)
	s.MaxRounds = 3
	// RequeueHook left nil.

	out, err := s.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Reason != "max_rounds" || out.Rounds != 3 {
		t.Errorf("nil-hook red run = (%q,%d), want (max_rounds,3) — baseline path changed", out.Reason, out.Rounds)
	}
	if out.Done {
		t.Error("a red run must not report Done")
	}
}

// TestRequeueHookNotConsultedOnGreen: a passing verifier converges; the hook is NEVER
// touched (it lives only in the !rep.Passed branch).
func TestRequeueHookNotConsultedOnGreen(t *testing.T) {
	m := &scriptModel{responses: repeatFinish(2)}
	s := baseSup(m, passVerifier{})

	var consulted int32
	s.RequeueHook = func(context.Context) ([]string, bool) {
		atomic.AddInt32(&consulted, 1)
		return nil, false
	}

	out, err := s.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done || out.Reason != "converged" {
		t.Errorf("green run = (Done=%v,%q), want (true,converged)", out.Done, out.Reason)
	}
	if consulted != 0 {
		t.Errorf("hook consulted %d times on a GREEN run, want 0", consulted)
	}
}

// TestRequeueHookExhaustedConvergesRed: the hook reporting exhausted stops the loop on
// the FIRST convergence-red — no extra round, no spinning to MaxRounds. The model has
// many more finish responses that must NEVER be reached.
func TestRequeueHookExhaustedConvergesRed(t *testing.T) {
	fv := &failVerifier{}
	m := &scriptModel{responses: repeatFinish(8)}
	s := baseSup(m, fv)
	s.MaxRounds = 20

	var consulted int32
	s.RequeueHook = func(context.Context) ([]string, bool) {
		atomic.AddInt32(&consulted, 1)
		return nil, true // every unit hit its ceiling
	}

	out, err := s.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Done {
		t.Error("exhausted requeue must converge RED, not Done")
	}
	if out.Reason != "requeue_exhausted" {
		t.Errorf("reason = %q, want requeue_exhausted", out.Reason)
	}
	if out.Rounds != 1 {
		t.Errorf("converged after %d rounds, want 1 (no extra round on exhausted)", out.Rounds)
	}
	if consulted != 1 {
		t.Errorf("hook consulted %d times, want exactly 1 (one convergence-red, no infinite consult)", consulted)
	}
	// The verifier ran exactly once: exhausted stopped before any re-verify.
	if fv.n != 1 {
		t.Errorf("verifier ran %d times, want 1 (no re-verify after exhausted)", fv.n)
	}
}

// TestRequeueHookFocusedRoundThenConverges is the headline: a red finish consults the
// hook, which reports a remaining (non-exhausted) unit; the loop runs one MORE focused
// round; the remaining unit ids are folded into the next turn as TRUSTED control text;
// and a re-derive makes the verifier pass so the run converges. The hook is consulted
// exactly once per convergence-red (here: once, since the second finish greens).
func TestRequeueHookFocusedRoundThenConverges(t *testing.T) {
	// Pass on the 2nd Check: finish #1 → red (hook fires, remaining unit) → finish #2 →
	// green → converged.
	v := &greenAfterN{n: 2}
	m := &scriptModel{responses: repeatFinish(8)}
	s := baseSup(m, v)
	s.MaxRounds = 20

	var consulted int32
	s.RequeueHook = func(context.Context) ([]string, bool) {
		atomic.AddInt32(&consulted, 1)
		return []string{"company-041-revenue", "company-041-margin"}, false
	}

	out, err := s.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done || out.Reason != "converged" {
		t.Errorf("run = (Done=%v,%q), want (true,converged) after a focused requeue round", out.Done, out.Reason)
	}
	if consulted != 1 {
		t.Errorf("hook consulted %d times, want exactly 1 (one convergence-red)", consulted)
	}
	// The focused requeue directive — the hook's TRUSTED unit ids — must have been folded
	// into a turn the model saw on the next (focused) round.
	if !lastMsgsContain(m, "company-041-revenue") || !lastMsgsContain(m, "company-041-margin") {
		t.Error("the remaining requeue unit ids were not folded into the focused round's prompt")
	}
}

// TestRequeueHookConsultedOncePerConvergenceRed: across TWO distinct convergence-reds
// (red finish, focused round, red finish again, then green), the hook fires exactly
// once per red — never more (no infinite consult inside a single red).
func TestRequeueHookConsultedOncePerConvergenceRed(t *testing.T) {
	v := &greenAfterN{n: 3} // red, red, then pass on the 3rd finish
	m := &scriptModel{responses: repeatFinish(8)}
	s := baseSup(m, v)
	s.MaxRounds = 20

	var consulted int32
	s.RequeueHook = func(context.Context) ([]string, bool) {
		atomic.AddInt32(&consulted, 1)
		return []string{"unit-x"}, false
	}

	out, err := s.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done {
		t.Fatalf("expected eventual convergence, got Done=%v reason=%q", out.Done, out.Reason)
	}
	// Two convergence-reds (Checks 1 and 2) ⇒ exactly two consults; Check 3 is green ⇒
	// no consult.
	if consulted != 2 {
		t.Errorf("hook consulted %d times, want exactly 2 (one per convergence-red, none on green)", consulted)
	}
}

// --- helpers ----------------------------------------------------------------

// repeatFinish builds n identical finish responses, so a test can drive as many
// convergence-red rounds as it needs without re-listing the script.
func repeatFinish(n int) []model.Response {
	out := make([]model.Response, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, textResp(toolUse("u", "finish", map[string]string{"summary": "done"})))
	}
	return out
}

// lastMsgsContain reports whether any text block in the model's last-seen messages
// contains s — used to assert the trusted focused-requeue directive reached the model.
func lastMsgsContain(m *scriptModel, s string) bool {
	for _, msg := range m.lastMsgs {
		for _, b := range msg.Content {
			if b.Type == "text" && strings.Contains(b.Text, s) {
				return true
			}
		}
	}
	return false
}
