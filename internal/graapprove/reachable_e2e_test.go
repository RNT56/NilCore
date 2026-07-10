package graapprove

import (
	"fmt"
	"testing"
	"time"

	"nilcore/internal/policy"
)

// presetNames are the envelopes an operator can actually select via
// NILCORE_AUTOAPPROVE_PRESET / config. Whatever they are, they must be REACHABLE.
func presetNames() []string { return []string{"cautious", "balanced", "trusted"} }

// greenRunVarying returns n passing boundary_outcomes for `action` whose scopes are all
// DISTINCT but share one family — exactly what a real run history looks like, since
// watch/schedule open PRs from "task/trig-<unix-nano>" and every branch is minted once.
func greenRunVarying(action, prefix string, n int) []logEntry {
	out := make([]logEntry, n)
	for i := range out {
		out[i] = logEntry{action: action, scope: fmt.Sprintf("%s-%d", prefix, 1720512345+i), passed: true}
	}
	return out
}

// THE reachability regression. With the shipped preset (AllowBranches:["*"]) and REAL
// per-run-unique task branches, a graded approver that has earned plenty of verifier-green
// history must actually auto-approve. Before the fix it denied every time — path.Match's
// `*` never matched a slash-y scope (out_of_scope), and even with that fixed the trust
// tally keyed on the unique branch could never reach MinSuccesses (below_bar).
func TestGradedApprovalIsReachableWithRealBranchScopes(t *testing.T) {
	for _, name := range presetNames() {
		t.Run(name, func(t *testing.T) {
			env, err := Preset(name)
			if err != nil {
				t.Skipf("preset %q not available: %v", name, err)
			}
			clause, ok := clauseOf(env, "open-pr")
			if !ok {
				t.Skipf("preset %q has no open-pr clause", name)
			}

			dir := t.TempDir()
			now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

			// Earn well past the bar, on distinct branches of one family.
			history := greenRunVarying("open-pr", "task/trig", clause.MinSuccesses+clause.MinSample+2)
			path := writeLog(t, dir, history)

			human := &recHuman{reply: false} // must not be consulted
			sink := &recSink{}
			g := newGraded(human, env, path, nil, WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

			// A fresh, never-before-seen task branch — the only kind that ever occurs.
			act := policy.GateAction{Type: policy.OpenPR, Branch: "task/trig-1720599999"}
			if !g.ApproveStructured(act) {
				ev, _ := sink.last()
				t.Fatalf("preset %q denied a fully-earned open-pr on a real task branch (reason=%v) — graduated auto-approval is unreachable", name, ev.detail)
			}
			if human.called {
				t.Fatal("human must not be consulted on an auto-approval")
			}
		})
	}
}

// The floor is independent of the envelope: `*` admits any branch, but a protected base
// must still fall through to the human no matter how much trust was earned.
func TestProtectedBaseStillFallsThroughDespiteTrust(t *testing.T) {
	env, err := Preset("trusted")
	if err != nil {
		t.Skipf("trusted preset unavailable: %v", err)
	}
	dir := t.TempDir()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	path := writeLog(t, dir, greenRunVarying("open-pr", "task/trig", 40))

	for _, base := range []string{"main", "master", "release/1.2", "prod", "trunk", "stable"} {
		human := &recHuman{reply: false}
		sink := &recSink{}
		g := newGraded(human, env, path, nil, WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

		if g.ApproveStructured(policy.GateAction{Type: policy.OpenPR, Branch: base}) {
			t.Errorf("auto-approved a protected base %q — charter violation", base)
		}
		if !human.called {
			t.Errorf("protected base %q must fall through to the human, not silently deny", base)
		}
	}
}

// An action with no scope has no bounded blast radius: never auto-approve.
func TestEmptyScopeFallsThroughDespiteTrust(t *testing.T) {
	env, err := Preset("trusted")
	if err != nil {
		t.Skipf("trusted preset unavailable: %v", err)
	}
	dir := t.TempDir()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	path := writeLog(t, dir, greenRunVarying("open-pr", "task/trig", 40))

	human := &recHuman{reply: false}
	g := newGraded(human, env, path, nil, WithClock(fixedClock(now)), WithRoot(dir))
	if g.ApproveStructured(policy.GateAction{Type: policy.OpenPR, Branch: ""}) {
		t.Fatal("auto-approved an action with an empty scope")
	}
}

// The per-day cap must actually BIND across distinct branches. With a per-exact-branch
// window every fresh task branch opened a new window and MaxPerDay never limited
// anything — the same ephemeral-scope bug, on the rate axis.
func TestRateCapBindsAcrossDistinctBranchesOfOneFamily(t *testing.T) {
	env := Envelope{Classes: []ClassClause{{
		Type:          "open-pr",
		AllowBranches: []string{"*"},
		DenyBranches:  commonDeny,
		MinSuccesses:  2,
		MinSample:     2,
		RecencyDays:   7,
		MaxPerDay:     2, // only two auto-approvals per day for this family
	}}}

	dir := t.TempDir()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	path := writeLog(t, dir, greenRunVarying("open-pr", "task/trig", 10))

	human := &recHuman{reply: false}
	g := newGraded(human, env, path, nil, WithClock(fixedClock(now)), WithRoot(dir))

	approved := 0
	for i := 0; i < 5; i++ {
		act := policy.GateAction{Type: policy.OpenPR, Branch: fmt.Sprintf("task/trig-90%d", i)}
		if g.ApproveStructured(act) {
			approved++
		}
	}
	if approved != 2 {
		t.Fatalf("auto-approved %d of 5 distinct task branches, want 2 (MaxPerDay must bind per family, not per branch)", approved)
	}
	if !human.called {
		t.Error("the rate-exceeded decisions must fall through to the human")
	}
}

// The per-day DOLLAR fence must bind across distinct branches too. Keyed on the exact
// branch, every fresh task/trig-<nano> opened a new $ window and MaxDollarsDay never
// limited anything — the same ephemeral-scope bug on the money axis.
func TestDayDollarCapBindsAcrossDistinctBranchesOfOneFamily(t *testing.T) {
	dir := t.TempDir()
	// writeLog stamps events with the real clock, and the $ seed only sums TODAY's
	// events — so the decision clock must be today too (as the sibling ceiling test does).
	now := time.Now().UTC()

	// Seed the day's spend from the durable log under DIFFERENT branches of one family.
	entries := greenRunVarying("open-pr", "task/trig", 10)
	entries = append(entries,
		logEntry{kind: "auto_approve", action: "open-pr", scope: "task/trig-1", passed: true, dollars: 4.0},
		logEntry{kind: "auto_approve", action: "open-pr", scope: "task/trig-2", passed: true, dollars: 4.0},
	)
	path := writeLog(t, dir, entries)

	withPerActionCost(t, 2) // this action costs $2 ⇒ 8+2 = 10 > the $5 ceiling

	env := Envelope{Classes: []ClassClause{{
		Type:          "open-pr",
		AllowBranches: []string{"*"},
		DenyBranches:  commonDeny,
		MinSuccesses:  2,
		MinSample:     2,
		RecencyDays:   7,
		MaxPerDay:     50,  // not the binding constraint here
		MaxDollarsDay: 5.0, // already $8 auto-approved today across the family
	}}}

	human := &recHuman{reply: false}
	g := newGraded(human, env, path, nil, WithClock(fixedClock(now)), WithRoot(dir))

	// A brand-new branch of the SAME family must see the family's spent budget.
	if g.ApproveStructured(policy.GateAction{Type: policy.OpenPR, Branch: "task/trig-9999"}) {
		t.Fatal("auto-approved past MaxDollarsDay — the $ window must key on the family, not the branch")
	}
	if !human.called {
		t.Error("a budget-exceeded decision must fall through to the human")
	}
}

// clauseOf finds a class clause by type.
func clauseOf(e Envelope, typ string) (ClassClause, bool) {
	for _, c := range e.Classes {
		if c.Type == typ {
			return c, true
		}
	}
	return ClassClause{}, false
}
