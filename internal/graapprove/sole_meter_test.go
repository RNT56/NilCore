package graapprove

// XC-T01 — prove internal/blastbudget is the SOLE per-day dollar meter the
// GradedApprover consults: a single shared $/rate/irreversible meter that cannot
// drift against a second, hidden counter. These tests pin the composition law from
// docs/ROADMAP-CLOSED-LOOP.md §6/§7:
//
//   - an auto_approve charges its clause's MaxDollarsDay THROUGH the injected
//     *blastbudget.Budget, and the charge is observable on that one budget
//     (blast.Used(day).Dollars) — never on a second counter,
//   - a budget already AT its ceiling overrides the (otherwise-granted) P5
//     decision and forces fall-through to the human (min(P5, blast); a blast
//     breach is final),
//   - a clause with a POSITIVE $/day ceiling but NO wired meter FAILS CLOSED:
//     the ceiling cannot be charged, so the action is denied + delegated rather
//     than silently auto-approved with no dollar accounting (B3-audit-verify.4).
//     A clause with no $ ceiling (MaxDollarsDay==0) needs no meter and stays a
//     byte-identical no-op when the budget is nil.
//
// SCOPE NOTE (current seam): the GradedApprover routes ONLY the per-day DOLLAR axis
// through blast (graded.go step 5b → ChargeAutoApprovalDollars). The per-day RATE
// cap (MaxPerDay) is metered by replaying the append-only log (countAutoApprovalsToday),
// i.e. one log-derived counter — not a parallel in-memory tally that could drift.
// The IRREVERSIBLE-count axis the roadmap also assigns to blastbudget (§7) is NOT
// charged from this package; per §6 it is fenced at the gate-path choke-point
// (BR-T04, internal/agent/orchestrator.go), not inside graapprove. That remaining
// wiring point is documented in the returned notes, not papered over here: these
// tests assert the strongest property the current seam actually provides — the
// dollar ceiling is enforced through exactly one shared meter.

import (
	"context"
	"testing"
	"time"

	"nilcore/internal/blastbudget"
	"nilcore/internal/policy"
)

// deployEnv is a single deploy clause earned at a low bar with a $25/day ceiling on
// the staging environment, so a small synthesized log clears the trust bar and the
// only remaining gate is the dollar charge through blastbudget.
func deployEnv() Envelope {
	return Envelope{Classes: []ClassClause{{
		Type:          "deploy",
		AllowBranches: []string{"staging"},
		DenyBranches:  commonDeny,
		Environments:  []string{"staging"},
		MinSuccesses:  2,
		MinSample:     2,
		RecencyDays:   7,
		MaxPerDay:     5,
		MaxDollarsDay: 25,
	}}}
}

func deployStaging() policy.GateAction {
	return policy.GateAction{Type: policy.Deploy, Branch: "staging"}
}

// chargedDollars extracts the "charged" field the auto_approve evidence records, so
// a test can assert the same value the production code routed to blastbudget.
func chargedDollars(t *testing.T, ev recEvent) float64 {
	t.Helper()
	d, ok := ev.detail["dollars"].(map[string]any)
	if !ok {
		t.Fatalf("auto_approve evidence missing dollars map: %+v", ev.detail)
	}
	v, ok := d["charged"].(float64)
	if !ok {
		t.Fatalf("auto_approve dollars.charged not a float64: %+v", d)
	}
	return v
}

// TestBlastBudgetUnmeteredDollarCeilingFailsClosed pins the DEFAULT (unwired) path:
// with a nil *blastbudget.Budget but a clause that declares a POSITIVE $/day ceiling,
// the GradedApprover must NOT silently auto-approve (that would enforce no dollar
// accounting at all). It fails closed — deny + delegate to the human with reason
// dollar_ceiling_unmetered — so an operator's intended $ ceiling is never a no-op
// (B3-audit-verify.4). A $-free clause (covered separately) stays a byte-identical
// no-op when the budget is nil.
func TestBlastBudgetUnmeteredDollarCeilingFailsClosed(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	path := writeLog(t, dir, greenRun("deploy", "staging", 3))

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, deployEnv(), path, nil, // <-- nil budget, but the clause has $25/day
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if g.ApproveStructured(deployStaging()) {
		t.Fatal("an unmetered positive $ ceiling must not auto-approve")
	}
	if !human.called {
		t.Fatal("an unmetered $ ceiling must fall through to the human")
	}
	assertDenyReason(t, sink, "dollar_ceiling_unmetered")
}

// TestBlastBudgetNoDollarCeilingNilBudgetIsNoOp proves the byte-identical default for
// a clause WITHOUT a $ ceiling: a nil budget is a pure no-op — the fully-earned action
// still auto-approves, the human is not consulted, and the full evidence object is
// emitted (charged $0). Only a positive ceiling demands a meter; a zero ceiling does not.
func TestBlastBudgetNoDollarCeilingNilBudgetIsNoOp(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	path := writeLog(t, dir, greenRun("deploy", "staging", 3))

	env := deployEnv()
	env.Classes[0].MaxDollarsDay = 0 // no $ ceiling ⇒ no meter required

	human := &recHuman{reply: false} // must NOT be consulted on a pass
	sink := &recSink{}
	g := newGraded(human, env, path, nil, // <-- nil budget: default-off seam
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if !g.ApproveStructured(deployStaging()) {
		t.Fatal("default path (nil budget, no $ ceiling) must still auto-approve a fully-earned action")
	}
	if human.called {
		t.Fatal("default path must not consult the human on an auto-approval")
	}

	ev, ok := sink.last()
	if !ok || ev.kind != "auto_approve" {
		t.Fatalf("expected an auto_approve event, got %+v", sink.events)
	}
	for _, k := range []string{"green", "total", "last_green", "bar", "rate", "dollars", "chain_ok"} {
		if _, ok := ev.detail[k]; !ok {
			t.Errorf("auto_approve evidence missing %q: %+v", k, ev.detail)
		}
	}
	if got := chargedDollars(t, ev); got != 0 {
		t.Fatalf("evidence dollars.charged = %v, want 0 (no ceiling)", got)
	}
}

// TestBlastBudgetChargedThroughSharedMeter proves the per-day dollars are charged
// THROUGH the injected budget — the single shared meter — and that the charge is
// observable there (blast.Used(today).Dollars). A SECOND, independent budget that
// was never injected stays at zero: there is exactly one counter, and it is the one
// the operator wired. (No second meter that can drift.)
func TestBlastBudgetChargedThroughSharedMeter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	today := dayKey(now)
	path := writeLog(t, dir, greenRun("deploy", "staging", 3))

	shared := blastbudget.New()
	shared.SetAutoApprovalDollarCeiling(100) // generous: admits the $25 charge

	// A decoy meter the GradedApprover is NOT given — it must never move.
	decoy := blastbudget.New()
	decoy.SetAutoApprovalDollarCeiling(100)

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, deployEnv(), path, shared,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if !g.ApproveStructured(deployStaging()) {
		t.Fatal("earned, in-budget deploy must auto-approve")
	}

	// The charge landed on the SHARED meter, equal to the clause ceiling.
	if got := shared.Used(today).Dollars; got != 25 {
		t.Fatalf("shared budget Used(%s).Dollars = %v, want 25 (charged through the single meter)", today, got)
	}
	// The decoy budget the approver never held stays at zero — there is no second
	// counter the approver writes to.
	if got := decoy.Used(today).Dollars; got != 0 {
		t.Fatalf("decoy budget moved to %v — a second meter is being charged (drift hazard)", got)
	}
	// The evidence the approver emitted matches what it charged: one source of truth.
	ev, _ := sink.last()
	if got := chargedDollars(t, ev); got != shared.Used(today).Dollars {
		t.Fatalf("evidence dollars.charged=%v disagrees with the shared meter %v", got, shared.Used(today).Dollars)
	}
}

// TestBlastBudgetCeilingOverridesP5Grant proves the composition law min(P5, blast):
// a budget already at its per-day ceiling makes the dollar charge fail, which
// OVERRIDES an otherwise-granted P5 decision and forces fall-through to the human.
// The blast breach is final — earned trust does not buy past the fence.
func TestBlastBudgetCeilingOverridesP5Grant(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	today := dayKey(now)
	// Trust is fully earned (3 greens, bar=2): every gate up to the dollar charge
	// passes, so the ONLY thing that can deny is the blast ceiling.
	path := writeLog(t, dir, greenRun("deploy", "staging", 3))

	// Pre-fill the shared budget so the $25 clause charge cannot fit: ceiling $10,
	// already $10 spent today ⇒ ChargeAutoApprovalDollars refuses.
	blast := blastbudget.New()
	blast.SetAutoApprovalDollarCeiling(10)
	if err := blast.ChargeAutoApprovalDollars(context.Background(), today, 10); err != nil {
		t.Fatalf("setup charge to ceiling: %v", err)
	}

	human := &recHuman{reply: true} // human SAYS yes — proves the fall-through happened
	sink := &recSink{}
	g := newGraded(human, deployEnv(), path, blast,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	// The decision must NOT be a P5 auto_approve; it falls through to the human
	// (who replies true here, so the overall verdict is true BUT via the human).
	if got := g.ApproveStructured(deployStaging()); got != true {
		t.Fatal("over-ceiling deploy must fall through to the human (who replies true)")
	}
	if !human.called {
		t.Fatal("a blast breach must override the P5 grant and consult the human")
	}
	assertDenyReason(t, sink, "over_ceiling")

	// And the fence held: the refused charge recorded nothing past the ceiling. The
	// blastbudget is fail-closed, so the day total is unchanged at exactly $10.
	if got := blast.Used(today).Dollars; got != 10 {
		t.Fatalf("refused charge must record nothing: Used(%s).Dollars = %v, want 10", today, got)
	}
}

// TestBlastBudgetAccumulatesInOneMeter proves repeated auto-approvals accumulate
// monotonically in the SAME meter (no reset, no parallel tally), and that the meter
// is what stops further charges once the day ceiling is reached — even while the
// per-class MaxPerDay rate cap would still allow more. The dollar ceiling and the
// rate cap are independent gates, each backed by exactly one counter.
func TestBlastBudgetAccumulatesInOneMeter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	today := dayKey(now)
	path := writeLog(t, dir, greenRun("deploy", "staging", 3))

	// Ceiling admits exactly two $25 charges ($50), then refuses the third — while
	// MaxPerDay (5) would still permit it. The dollar fence, not the rate cap, is
	// what stops the third.
	blast := blastbudget.New()
	blast.SetAutoApprovalDollarCeiling(50)

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, deployEnv(), path, blast,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if !g.ApproveStructured(deployStaging()) {
		t.Fatal("first deploy must auto-approve")
	}
	if got := blast.Used(today).Dollars; got != 25 {
		t.Fatalf("after 1 approval Used().Dollars = %v, want 25", got)
	}
	if !g.ApproveStructured(deployStaging()) {
		t.Fatal("second deploy must auto-approve (still within $50)")
	}
	if got := blast.Used(today).Dollars; got != 50 {
		t.Fatalf("after 2 approvals Used().Dollars = %v, want 50 (one accumulating meter)", got)
	}

	// Third charge would be $75 > $50 ceiling ⇒ refused ⇒ human; the rate cap (5)
	// is NOT the reason. The single dollar meter holds the line.
	if got := g.ApproveStructured(deployStaging()); got != false {
		t.Fatal("third deploy must be refused by the dollar ceiling and fall to the human (reply=false)")
	}
	if !human.called {
		t.Fatal("the dollar-ceiling refusal must consult the human")
	}
	assertDenyReason(t, sink, "over_ceiling")
	if got := blast.Used(today).Dollars; got != 50 {
		t.Fatalf("refused third charge must record nothing: Used().Dollars = %v, want 50", got)
	}
}
