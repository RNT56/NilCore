package graapprove

// XC-T01 — prove the graduated-auto-approval $ axis is metered by exactly ONE per-day
// authority per (type,scope) and is REACHABLE, BOUNDED, and FAIL-CLOSED. These tests pin
// the corrected composition law from docs/ROADMAP-CLOSED-LOOP.md §6/§7 after the
// cumulative-ledger regression:
//
//   - clause.MaxDollarsDay is the per-UTC-day CEILING on ACTUAL auto-approved spend, NOT
//     the cost of one action, and NOT a per-action delta of the run ledger. A single
//     action charges its OWN cost (perActionCost — $0 today, since no GateAction field
//     carries a per-action figure); Evidence.SpentUSD (the run ledger's CUMULATIVE total)
//     is NEVER charged as this action's cost. That cumulative-ledger premise was the
//     regression: under the default -blast-radius off it denied every action once the run
//     had spent any money.
//   - the per-day auto-approved-$ total is tracked in-process (dollarsDay), seeded once per
//     window from the durable log (sumAutoApprovalDollarsToday, which sums each auto_approve
//     event's dollars.actual_usd), so a restart never resets it — the SAME log-replay
//     discipline the MaxPerDay rate axis uses.
//   - when a blast budget is present, the SAME charge routes through it
//     (ChargeAutoApprovalDollars), so the effective ceiling is min(clause.MaxDollarsDay,
//     blast day ceiling): the tighter meter bites first, and the two never disagree (the
//     in-process total is committed only when the shared charge also lands).
//   - the DEFAULT g.blast==nil path is reachable and metered by the clause's OWN
//     MaxDollarsDay — we never blanket-deny "unmetered" just because the run has spent money.
//
// SCOPE NOTE (current seam): the GradedApprover routes the per-day DOLLAR axis through both
// the in-process dollarsDay counter (clause ceiling) and, when present, blast
// (min-with-blast). The per-day RATE cap (MaxPerDay) is metered by replaying the append-only
// log (countAutoApprovalsToday). The IRREVERSIBLE-count axis is fenced at the gate-path
// choke-point (BR-T04, internal/agent/orchestrator.go), not inside graapprove.

import (
	"context"
	"testing"
	"time"

	"nilcore/internal/blastbudget"
	"nilcore/internal/policy"
)

// deployEnv is a single deploy clause earned at a low bar with a $25/day ceiling on the
// staging environment, so a small synthesized log clears the trust bar and the only
// remaining gate is the per-day dollar ceiling.
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

// withPerActionCost overrides the (normally $0) per-action cost for the duration of a test
// so the ceiling can actually be driven. Production never reassigns perActionCost; this
// seam exists precisely because no GateAction field carries a per-action $ figure yet.
func withPerActionCost(t *testing.T, cost float64) {
	t.Helper()
	prev := perActionCost
	perActionCost = func(policy.GateAction) float64 { return cost }
	t.Cleanup(func() { perActionCost = prev })
}

// chargedDollars extracts the "charged" field the auto_approve evidence records, so a test
// can assert the same value the production code routed to the meters.
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

func mustLast(t *testing.T, s *recSink) recEvent {
	t.Helper()
	ev, ok := s.last()
	if !ok {
		t.Fatal("expected an emitted event")
	}
	return ev
}

// TestUnmeteredZeroCostReachable proves the REGRESSION FIX: with a nil *blastbudget.Budget
// (default -blast-radius off) and a clause that declares a POSITIVE $/day ceiling, a fully
// earned action whose OWN cost is $0 (the case today) auto-approves — it is NOT blanket-denied
// "unmetered" merely because the clause has a ceiling or the run has spent money. Before the
// fix this path denied dollar_ceiling_unmetered whenever the cumulative run ledger was >0,
// disabling graduated auto-approval in its default mode.
func TestUnmeteredZeroCostReachable(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	path := writeLog(t, dir, greenRun("deploy", "staging", 3))

	human := &recHuman{reply: false} // must NOT be consulted on a real auto-approval
	sink := &recSink{}
	g := newGraded(human, deployEnv(), path, nil, // nil budget + positive-ceiling clause
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	// A cumulative run spend of $500 on prior work must NOT be charged as this action's cost.
	act := deployStaging()
	act.Evidence = &policy.GateEvidence{SpentUSD: 500}
	if !g.ApproveStructured(act) {
		t.Fatalf("a $0-cost deploy must auto-approve under a nil meter (last: %+v)", mustLast(t, sink).detail)
	}
	if human.called {
		t.Fatal("a reachable auto-approval must not consult the human")
	}
	if got := chargedDollars(t, mustLast(t, sink)); got != 0 {
		t.Fatalf("evidence dollars.charged = %v, want 0 (cumulative ledger is never this action's cost)", got)
	}
}

// TestNoDollarCeilingNilBudgetIsNoOp proves the byte-identical default for a clause WITHOUT
// a $ ceiling (MaxDollarsDay==0): a nil budget is a pure no-op — a fully-earned $0-cost
// action still auto-approves, the human is not consulted, and the full evidence object is
// emitted (charged $0). Only a positive per-action cost demands accounting; a zero ceiling
// with a zero cost is unconstrained.
func TestNoDollarCeilingNilBudgetIsNoOp(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	path := writeLog(t, dir, greenRun("deploy", "staging", 3))

	env := deployEnv()
	env.Classes[0].MaxDollarsDay = 0 // no $ ceiling

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, env, path, nil, // nil budget: default-off seam
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if !g.ApproveStructured(deployStaging()) {
		t.Fatal("default path (nil budget, no $ ceiling, $0 cost) must still auto-approve")
	}
	if human.called {
		t.Fatal("default path must not consult the human on an auto-approval")
	}
	ev := mustLast(t, sink)
	if ev.kind != "auto_approve" {
		t.Fatalf("expected an auto_approve event, got %+v", sink.events)
	}
	for _, k := range []string{"green", "total", "last_green", "bar", "rate", "dollars", "chain_ok"} {
		if _, ok := ev.detail[k]; !ok {
			t.Errorf("auto_approve evidence missing %q: %+v", k, ev.detail)
		}
	}
	if got := chargedDollars(t, ev); got != 0 {
		t.Fatalf("evidence dollars.charged = %v, want 0 (no ceiling, $0 cost)", got)
	}
}

// TestZeroCeilingPositiveCostDenied proves MaxDollarsDay==0 means "$-unbounded not allowed":
// a clause with a zero ceiling denies any action whose own cost is positive (there is no
// budget for it), while a $0-cost action still passes. This keeps a zero ceiling meaningful
// rather than silently unbounded.
func TestZeroCeilingPositiveCostDenied(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	path := writeLog(t, dir, greenRun("deploy", "staging", 3))

	env := deployEnv()
	env.Classes[0].MaxDollarsDay = 0 // zero ceiling ⇒ no positive spend permitted

	withPerActionCost(t, 1) // a positive cost against a zero ceiling ⇒ deny

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, env, path, nil,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if g.ApproveStructured(deployStaging()) {
		t.Fatal("a positive-cost action under a zero $ ceiling must not auto-approve")
	}
	assertDenyReason(t, sink, "over_ceiling")
}

// TestClauseCeilingBitesWithoutBlast proves the clause MaxDollarsDay is a REAL per-day
// ceiling even with NO blast meter (g.blast==nil). The day total is seeded from the durable
// log ($24 of prior auto-approved spend today), and a $2 action would push it to $26 > the
// $25 clause ceiling ⇒ deny over_ceiling and delegate. This is (2) + (b) on the blast==nil path.
func TestClauseCeilingBitesWithoutBlast(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	// Prior auto-approved spend today: $24 (via a durable auto_approve event carrying dollars).
	entries := append(greenRun("deploy", "staging", 3),
		logEntry{kind: "auto_approve", action: "deploy", scope: "staging", passed: true, dollars: 24})
	path := writeLog(t, dir, entries)

	withPerActionCost(t, 2) // this action costs $2 ⇒ 24+2 = 26 > 25 ceiling

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, deployEnv(), path, nil, // NO blast meter — the clause alone must bite
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if g.ApproveStructured(deployStaging()) {
		t.Fatal("an action pushing the day total over the clause ceiling must not auto-approve")
	}
	if !human.called {
		t.Fatal("an over-ceiling action must delegate to the human")
	}
	assertDenyReason(t, sink, "over_ceiling")
}

// TestClauseCeilingReachableWithoutBlast is the positive companion: with $22 of prior
// auto-approved spend today and a $2 action (total $24 <= $25), the action auto-approves
// under a nil meter — the clause ceiling is REACHABLE, not a blanket deny.
func TestClauseCeilingReachableWithoutBlast(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	entries := append(greenRun("deploy", "staging", 3),
		logEntry{kind: "auto_approve", action: "deploy", scope: "staging", passed: true, dollars: 22})
	path := writeLog(t, dir, entries)

	withPerActionCost(t, 2) // 22+2 = 24 <= 25 ceiling ⇒ fits

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, deployEnv(), path, nil,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if !g.ApproveStructured(deployStaging()) {
		t.Fatalf("an in-budget action must auto-approve under a nil meter (last: %+v)", mustLast(t, sink).detail)
	}
	if human.called {
		t.Fatal("an in-budget auto-approval must not consult the human")
	}
	if got := chargedDollars(t, mustLast(t, sink)); got != 2 {
		t.Fatalf("evidence dollars.charged = %v, want 2 (this action's own cost)", got)
	}
}

// TestChargedThroughSharedMeter proves the per-day dollars are charged THROUGH the injected
// budget — the single shared meter — and are observable there (blast.Used(today).Dollars). A
// SECOND, independent budget that was never injected stays at zero: exactly one shared
// counter, plus the in-process clause counter, and they agree.
func TestChargedThroughSharedMeter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	today := dayKey(now)
	path := writeLog(t, dir, greenRun("deploy", "staging", 3))

	shared := blastbudget.New()
	shared.SetAutoApprovalDollarCeiling(100) // generous: admits the $3 charge

	decoy := blastbudget.New() // never given to the approver — must never move
	decoy.SetAutoApprovalDollarCeiling(100)

	withPerActionCost(t, 3) // this action costs $3

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, deployEnv(), path, shared,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if !g.ApproveStructured(deployStaging()) {
		t.Fatal("earned, in-budget deploy must auto-approve")
	}
	if got := shared.Used(today).Dollars; got != 3 {
		t.Fatalf("shared budget Used(%s).Dollars = %v, want 3 (charged through the single meter)", today, got)
	}
	if got := decoy.Used(today).Dollars; got != 0 {
		t.Fatalf("decoy budget moved to %v — a second meter is being charged (drift hazard)", got)
	}
	if got := chargedDollars(t, mustLast(t, sink)); got != shared.Used(today).Dollars {
		t.Fatalf("evidence dollars.charged=%v disagrees with the shared meter %v", got, shared.Used(today).Dollars)
	}
}

// TestMinOfClauseAndBlastCeiling proves (d): with a blast budget present, the effective
// ceiling is min(clause.MaxDollarsDay, blast dayCeil). The clause allows $25/day but the
// blast day ceiling is $5, already $4 spent ⇒ a $2 action ($4+$2=$6 > $5) is refused by the
// BLAST fence even though it is well within the $25 clause ceiling. The tighter meter wins.
func TestMinOfClauseAndBlastCeiling(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	today := dayKey(now)
	path := writeLog(t, dir, greenRun("deploy", "staging", 3))

	blast := blastbudget.New()
	blast.SetAutoApprovalDollarCeiling(5) // blast day ceiling < clause $25
	if err := blast.ChargeAutoApprovalDollars(context.Background(), today, 4); err != nil {
		t.Fatalf("setup charge: %v", err)
	}

	withPerActionCost(t, 2) // 4 (blast) + 2 = 6 > 5 blast ceiling (but < 25 clause)

	human := &recHuman{reply: true} // says yes ⇒ proves the fall-through happened
	sink := &recSink{}
	g := newGraded(human, deployEnv(), path, blast,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if got := g.ApproveStructured(deployStaging()); got != true {
		t.Fatal("an over-blast-ceiling deploy must fall through to the human (who replies true)")
	}
	if !human.called {
		t.Fatal("the tighter blast ceiling must override the P5 grant and consult the human")
	}
	assertDenyReason(t, sink, "over_ceiling")
	// Fail-closed: the refused charge recorded nothing; the blast day total is unchanged.
	if got := blast.Used(today).Dollars; got != 4 {
		t.Fatalf("refused charge must record nothing: Used(%s).Dollars = %v, want 4", today, got)
	}
}

// TestPerDayTotalAccumulatesInProcess proves the per-day total accumulates across successive
// in-process auto-approvals in ONE meter: two $2 deploys bring the shared meter to $4, and a
// third that would breach the (tight) blast ceiling is refused from the accumulated baseline,
// not from zero.
func TestPerDayTotalAccumulatesInProcess(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	today := dayKey(now)
	path := writeLog(t, dir, greenRun("deploy", "staging", 3))

	blast := blastbudget.New()
	blast.SetAutoApprovalDollarCeiling(5) // admits two $2 charges ($4), refuses the third ($6)
	withPerActionCost(t, 2)

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, deployEnv(), path, blast,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	// Two separate deploys — evaluate both (no || short-circuit) so BOTH charge the meter
	// (the $4 assertion below depends on it) and each result is checked independently.
	ok1 := g.ApproveStructured(deployStaging())
	ok2 := g.ApproveStructured(deployStaging())
	if !ok1 || !ok2 {
		t.Fatal("two in-budget deploys must auto-approve")
	}
	if got := blast.Used(today).Dollars; got != 4 {
		t.Fatalf("after two $2 charges, meter = %v, want 4 (one accumulating meter)", got)
	}
	// Third $2 ⇒ 4+2 = 6 > 5 ⇒ refused from the accumulated $4, not reset to zero.
	if g.ApproveStructured(deployStaging()) {
		t.Fatal("third deploy must be refused by the accumulated day total")
	}
	assertDenyReason(t, sink, "over_ceiling")
	if got := blast.Used(today).Dollars; got != 4 {
		t.Fatalf("refused third charge must record nothing: meter = %v, want 4", got)
	}
}

// TestPerDayTotalReseedsFromDurableLog proves a restart never resets the day's $ total: a
// brand-new approver reseeds the per-day auto-approved-dollar total from the durable log
// (the two prior auto_approve events carrying dollars.actual_usd), so it is bounded from the
// recovered baseline rather than from zero (mirroring the rate axis; no fail-open, I5).
func TestPerDayTotalReseedsFromDurableLog(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	today := dayKey(now)
	// Two prior auto-approved deploys today totalling $4 (durable), then the trust greens.
	entries := append([]logEntry{
		{kind: "auto_approve", action: "deploy", scope: "staging", passed: true, dollars: 2},
		{kind: "auto_approve", action: "deploy", scope: "staging", passed: true, dollars: 2},
	}, greenRun("deploy", "staging", 3)...)
	path := writeLog(t, dir, entries)

	// The reseed helper recovers exactly $4 for the (deploy,staging) window today.
	seed, err := sumAutoApprovalDollarsToday(path, "deploy", "staging", today)
	if err != nil {
		t.Fatalf("reseed read: %v", err)
	}
	if seed != 4 {
		t.Fatalf("reseeded day total = %v, want 4 (recovered from the durable log)", seed)
	}

	// A fresh approver (simulated restart) with the clause $25 ceiling: the recovered $4 plus
	// a $22 action would breach $25 ⇒ deny; the restart did NOT reset the day to zero.
	withPerActionCost(t, 22) // 4 (reseeded) + 22 = 26 > 25 clause ceiling
	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, deployEnv(), path, nil,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))
	if g.ApproveStructured(deployStaging()) {
		t.Fatal("a restarted approver must bound from the reseeded day total, not from zero")
	}
	assertDenyReason(t, sink, "over_ceiling")
}
