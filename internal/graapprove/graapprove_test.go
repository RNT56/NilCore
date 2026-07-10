package graapprove

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nilcore/internal/blastbudget"
	"nilcore/internal/eventlog"
	"nilcore/internal/policy"
)

// --- test doubles ---------------------------------------------------------

// recHuman records whether the human fall-through was consulted and what it was
// asked, and returns a fixed verdict.
type recHuman struct {
	called bool
	asked  string
	reply  bool
}

func (h *recHuman) Approve(action string) bool {
	h.called = true
	h.asked = action
	return h.reply
}

// recSink captures emitted audit events for assertions.
type recSink struct {
	events []recEvent
}

type recEvent struct {
	kind   string
	detail map[string]any
}

func (s *recSink) Emit(kind string, detail map[string]any) {
	s.events = append(s.events, recEvent{kind: kind, detail: detail})
}

func (s *recSink) last() (recEvent, bool) {
	if len(s.events) == 0 {
		return recEvent{}, false
	}
	return s.events[len(s.events)-1], true
}

// fixedClock returns a deterministic clock for recency/rate.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// writeLog builds a real hash-chained event log of boundary_outcome events and
// returns its path. Each entry is (action, scope, passed, time). The log is closed
// before return so Verify reads the durable file.
func writeLog(t *testing.T, dir string, entries []logEntry) string {
	t.Helper()
	path := filepath.Join(dir, "events.log")
	l, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	for _, e := range entries {
		detail := map[string]any{"action": e.action, "scope": e.scope, "passed": e.passed}
		if e.kind == "" {
			e.kind = "boundary_outcome"
		}
		// An auto_approve event carries a dollars map (as the production emit does), so a
		// test can seed the per-day auto-approved-$ total the reseed path (sumAutoApprovalDollarsToday)
		// reads back. dollars is only meaningful on auto_approve entries.
		if e.kind == "auto_approve" && e.dollars > 0 {
			detail["dollars"] = map[string]any{"charged": e.dollars, "actual_usd": e.dollars}
		}
		l.Append(eventlog.Event{Kind: e.kind, Detail: detail})
	}
	if err := l.Err(); err != nil {
		t.Fatalf("log write error: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("log close: %v", err)
	}
	return path
}

type logEntry struct {
	kind    string // default "boundary_outcome"
	action  string
	scope   string
	passed  bool
	dollars float64 // for kind=="auto_approve": the dollars.actual_usd this grant recorded
}

// greenRun returns n passing boundary_outcomes for (action,scope).
func greenRun(action, scope string, n int) []logEntry {
	out := make([]logEntry, n)
	for i := range out {
		out[i] = logEntry{action: action, scope: scope, passed: true}
	}
	return out
}

// --- Validate -------------------------------------------------------------

func TestValidateFailClosed(t *testing.T) {
	good := ClassClause{Type: "open-pr", AllowBranches: []string{"*"}, MinSuccesses: 5, MinSample: 5, RecencyDays: 14, MaxPerDay: 3}
	cases := []struct {
		name    string
		clause  ClassClause
		wantErr bool
	}{
		{"valid", good, false},
		{"unknown type", with(good, func(c *ClassClause) { c.Type = "rm-rf" }), true},
		{"empty type", with(good, func(c *ClassClause) { c.Type = "" }), true},
		{"min successes zero (blank bar rejected)", with(good, func(c *ClassClause) { c.MinSuccesses = 0 }), true},
		{"min sample below successes", with(good, func(c *ClassClause) { c.MinSample = 4 }), true},
		{"recency zero", with(good, func(c *ClassClause) { c.RecencyDays = 0 }), true},
		{"max per day zero", with(good, func(c *ClassClause) { c.MaxPerDay = 0 }), true},
		{"negative dollars", with(good, func(c *ClassClause) { c.MaxDollarsDay = -1 }), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := &Envelope{Classes: []ClassClause{tc.clause}}
			err := env.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}

	if err := (&Envelope{}).Validate(); err == nil {
		t.Fatal("empty envelope must be rejected by Validate")
	}
	var nilEnv *Envelope
	if err := nilEnv.Validate(); err == nil {
		t.Fatal("nil envelope must be rejected by Validate")
	}
}

func with(c ClassClause, f func(*ClassClause)) ClassClause {
	cp := c
	f(&cp)
	return cp
}

// --- Presets --------------------------------------------------------------

func TestPresetsNeverAdmitProtectedBranches(t *testing.T) {
	protected := []string{"main", "master", "release/v1", "release", "prod", "prod-east", "production"}
	for _, name := range []string{"conservative", "standard", "trusted"} {
		env, err := Preset(name)
		if err != nil {
			t.Fatalf("Preset(%q): %v", name, err)
		}
		if err := env.Validate(); err != nil {
			t.Fatalf("Preset(%q) does not validate: %v", name, err)
		}
		for _, clause := range env.Classes {
			for _, scope := range protected {
				// A protected scope must be denied: either prod*, or matched by a deny
				// glob, or simply not admitted by the allowlist.
				admitted := !isProd(scope) &&
					!matchAny(scope, clause.DenyBranches) &&
					matchAny(scope, clause.AllowBranches)
				if admitted {
					t.Errorf("preset %q class %q ADMITS protected scope %q (deny=%v allow=%v)",
						name, clause.Type, scope, clause.DenyBranches, clause.AllowBranches)
				}
			}
		}
	}
}

func TestPresetUnknownReturnsZeroAndError(t *testing.T) {
	for _, name := range []string{"", "yolo", "Conservative"} {
		env, err := Preset(name)
		if err == nil {
			t.Errorf("Preset(%q) expected error", name)
		}
		if len(env.Classes) != 0 {
			t.Errorf("Preset(%q) expected zero Envelope, got %v", name, env)
		}
	}
}

// --- BuildTrust -----------------------------------------------------------

func TestBuildTrustMissingLogIsCleanEmpty(t *testing.T) {
	view, err := BuildTrust(filepath.Join(t.TempDir(), "nope.log"))
	if err != nil {
		t.Fatalf("missing log must be a nil error, got %v", err)
	}
	if !view.ChainOK {
		t.Fatal("missing log must report ChainOK=true (clean empty)")
	}
	if got := view.Tally(ScopeKey{Type: "open-pr", Scope: "feat/x"}); got != (Tally{}) {
		t.Fatalf("missing log must have empty tallies, got %+v", got)
	}
}

func TestBuildTrustFolds(t *testing.T) {
	dir := t.TempDir()
	entries := append(greenRun("open-pr", "feat/x", 3),
		logEntry{action: "open-pr", scope: "feat/x", passed: false},
		// a different scope and an unrelated kind that must be ignored
		logEntry{action: "push", scope: "feat/y", passed: true},
		logEntry{kind: "auto_approve", action: "open-pr", scope: "feat/x", passed: true},
	)
	path := writeLog(t, dir, entries)
	view, err := BuildTrust(path)
	if err != nil {
		t.Fatalf("BuildTrust: %v", err)
	}
	if !view.ChainOK {
		t.Fatal("intact chain must report ChainOK=true")
	}
	got := view.Tally(ScopeKey{Type: "open-pr", Scope: "feat/x"})
	// 3 green + 1 fail = 4 total, 3 green; the auto_approve must NOT be counted.
	if got.Green != 3 || got.Total != 4 {
		t.Fatalf("expected 3 green / 4 total (auto_approve excluded), got %+v", got)
	}
	if got.LastGreen.IsZero() {
		t.Fatal("expected a non-zero LastGreen")
	}
}

func TestBuildTrustTamperFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 3))

	// Tamper: flip a byte in the middle of the file so the hash chain breaks.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// find a 'passed":true' and corrupt the action value, keeping it valid JSON-ish
	// by replacing within the scope string.
	corrupt := []byte(string(data))
	idx := indexOf(corrupt, []byte("feat/x"))
	if idx < 0 {
		t.Fatal("setup: scope token not found")
	}
	corrupt[idx] = 'Z'
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	view, err := BuildTrust(path)
	if err == nil {
		t.Fatal("tampered chain must return an error (deny explicitly)")
	}
	if view.ChainOK {
		t.Fatal("tampered chain must report ChainOK=false")
	}
	if got := view.Tally(ScopeKey{Type: "open-pr", Scope: "feat/x"}); got != (Tally{}) {
		t.Fatalf("tampered chain must yield EMPTY tallies, got %+v", got)
	}
}

func indexOf(hay, needle []byte) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// --- GradedApprover decision matrix --------------------------------------

// trustedEnv: a single open-pr clause earned at a low bar so a small synthesized
// log can clear it deterministically.
func trustedEnv() Envelope {
	return Envelope{Classes: []ClassClause{{
		Type:          "open-pr",
		AllowBranches: []string{"feat/*"},
		DenyBranches:  commonDeny,
		MinSuccesses:  2,
		MinSample:     2,
		RecencyDays:   7,
		MaxPerDay:     2,
	}}}
}

func openPR(scope string) policy.GateAction {
	return policy.GateAction{Type: policy.OpenPR, Branch: scope}
}

func TestApproveStructuredAllPass(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 3))

	human := &recHuman{reply: false} // must NOT be consulted on a pass
	sink := &recSink{}
	g := newGraded(human, trustedEnv(), path, nil,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if !g.ApproveStructured(openPR("feat/x")) {
		t.Fatal("all-pass action must auto-approve")
	}
	if human.called {
		t.Fatal("human must NOT be consulted on an auto-approval")
	}
	ev, ok := sink.last()
	if !ok || ev.kind != "auto_approve" {
		t.Fatalf("expected an auto_approve event, got %+v", sink.events)
	}
	// full evidence object present
	for _, k := range []string{"green", "total", "last_green", "bar", "rate", "dollars", "chain_ok"} {
		if _, ok := ev.detail[k]; !ok {
			t.Errorf("auto_approve evidence missing %q: %+v", k, ev.detail)
		}
	}
}

// selfAccEnv is an envelope that auto-approves the bind-self-authored class (the
// closed-loop self-acceptance gate) on any non-protected scope once it has earned trust.
func selfAccEnv() Envelope {
	return Envelope{Classes: []ClassClause{{
		Type:          "bind-self-authored",
		AllowBranches: []string{"*"},
		DenyBranches:  commonDeny,
		MinSuccesses:  2,
		MinSample:     2,
		RecencyDays:   7,
		MaxPerDay:     3,
	}}}
}

func bindSelf(scope string) policy.GateAction {
	return policy.GateAction{Type: policy.BindSelfAuthored, Branch: scope}
}

// TestApproveStructuredBindSelfAuthored proves the NEW bind-self-authored class end-to-
// end through the graded gate: an un-earned (id+command) scope falls to the human
// (deny-default headless), and the SAME scope auto-approves once it has earned trust —
// the amortization the closed-loop self-acceptance relies on.
func TestApproveStructuredBindSelfAuthored(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	scope := "candidate.build@abc123def456"

	// Fresh scope, empty log ⇒ below bar ⇒ falls to the human (here: deny).
	dir := t.TempDir()
	fresh := writeLog(t, dir, nil)
	human := &recHuman{reply: false}
	g := newGraded(human, selfAccEnv(), fresh, nil, WithClock(fixedClock(now)), WithRoot(dir))
	if g.ApproveStructured(bindSelf(scope)) {
		t.Fatal("an un-earned self-check scope must NOT auto-approve")
	}
	if !human.called {
		t.Fatal("an un-earned scope must fall to the human (deny-default)")
	}

	// Same scope after >= MinSuccesses greens ⇒ auto-approve without the human.
	dir2 := t.TempDir()
	earned := writeLog(t, dir2, greenRun("bind-self-authored", scope, 2))
	human2 := &recHuman{reply: false}
	g2 := newGraded(human2, selfAccEnv(), earned, nil, WithClock(fixedClock(now)), WithRoot(dir2))
	if !g2.ApproveStructured(bindSelf(scope)) {
		t.Fatal("an earned self-check scope must auto-approve within the envelope")
	}
	if human2.called {
		t.Fatal("an earned scope must NOT consult the human")
	}
}

// TestApproveStructuredProtectedBaseFloor proves the STRUCTURAL protected-base floor
// holds even for a sloppy custom envelope that ALLOWS main and supplies NO
// DenyBranches: a merge/promote onto main/master/release must never auto-approve,
// regardless of operator config (charter: graduated auto-approval never auto-approves
// main/prod). Without isProtectedBase this would auto-approve onto main.
func TestApproveStructuredProtectedBaseFloor(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	// A custom envelope that recklessly allows everything and denies nothing.
	reckless := Envelope{Classes: []ClassClause{{
		Type:          "open-pr",
		AllowBranches: []string{"*"},
		DenyBranches:  nil, // operator forgot to deny main/master/release
		MinSuccesses:  2,
		MinSample:     2,
		RecencyDays:   7,
		MaxPerDay:     5,
	}}}
	for _, scope := range []string{"main", "master", "release", "release/1.2", "release-2026"} {
		dir := t.TempDir()
		path := writeLog(t, dir, greenRun("open-pr", scope, 3))
		human := &recHuman{reply: false}
		g := newGraded(human, reckless, path, nil, WithClock(fixedClock(now)), WithRoot(dir))
		if g.ApproveStructured(openPR(scope)) {
			t.Errorf("scope %q: protected base must NOT auto-approve even when the envelope allows it", scope)
		}
		if !human.called {
			t.Errorf("scope %q: must fall through to the human gate", scope)
		}
	}
}

func TestApproveStructuredKillSwitchFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 3))

	// trip the sentinel file
	if err := os.MkdirAll(filepath.Join(dir, ".nilcore"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, defaultKillSwitchPath), []byte("off"), 0o644); err != nil {
		t.Fatal(err)
	}

	human := &recHuman{reply: true}
	sink := &recSink{}
	g := newGraded(human, trustedEnv(), path, nil,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if got := g.ApproveStructured(openPR("feat/x")); got != true {
		t.Fatal("kill-switch must delegate to human (who replies true here)")
	}
	if !human.called {
		t.Fatal("kill-switch must consult the human")
	}
	assertDenyReason(t, sink, "killswitch")
}

func TestApproveStructuredKillSwitchEnv(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 3))
	t.Setenv("NILCORE_AUTOAPPROVE_OFF", "1")

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, trustedEnv(), path, nil,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if g.ApproveStructured(openPR("feat/x")) {
		t.Fatal("env kill-switch must not auto-approve")
	}
	if !human.called {
		t.Fatal("env kill-switch must consult the human")
	}
	assertDenyReason(t, sink, "killswitch")
}

func TestApproveStructuredNotEligible(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	path := writeLog(t, dir, greenRun("push", "feat/x", 3))

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, trustedEnv(), path, nil, // env has open-pr only
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	// Push has no clause ⇒ not_eligible.
	if g.ApproveStructured(policy.GateAction{Type: policy.Push, Branch: "feat/x"}) {
		t.Fatal("ineligible class must not auto-approve")
	}
	if !human.called {
		t.Fatal("ineligible class must consult the human")
	}
	assertDenyReason(t, sink, "not_eligible")
}

func TestApproveStructuredOutOfScopeDeny(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	// earn trust on main (so the only failing gate is scope)
	path := writeLog(t, dir, greenRun("open-pr", "main", 3))

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, trustedEnv(), path, nil,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if g.ApproveStructured(openPR("main")) {
		t.Fatal("protected branch must never auto-approve")
	}
	if !human.called {
		t.Fatal("out-of-scope must consult the human")
	}
	assertDenyReason(t, sink, "out_of_scope")
}

func TestApproveStructuredOutOfScopeNotAllowed(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	path := writeLog(t, dir, greenRun("open-pr", "hotfix/z", 3))

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, trustedEnv(), path, nil, // allow is feat/* only
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if g.ApproveStructured(openPR("hotfix/z")) {
		t.Fatal("scope not on allowlist must not auto-approve")
	}
	assertDenyReason(t, sink, "out_of_scope")
}

func TestApproveStructuredBelowBar(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	// only 1 green — below MinSuccesses=2.
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 1))

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, trustedEnv(), path, nil,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if g.ApproveStructured(openPR("feat/x")) {
		t.Fatal("below-bar action must not auto-approve")
	}
	assertDenyReason(t, sink, "below_bar")
}

func TestApproveStructuredBelowBarStaleRecency(t *testing.T) {
	dir := t.TempDir()
	// log written "now", but the clock is 30 days later ⇒ recency fails.
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 3))
	later := time.Now().UTC().Add(30 * 24 * time.Hour)

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, trustedEnv(), path, nil,
		WithSink(sink), WithClock(fixedClock(later)), WithRoot(dir))

	if g.ApproveStructured(openPR("feat/x")) {
		t.Fatal("stale recency must not auto-approve")
	}
	assertDenyReason(t, sink, "below_bar")
}

func TestApproveStructuredChainBroken(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 3))

	// tamper the chain
	data, _ := os.ReadFile(path)
	idx := indexOf(data, []byte("feat/x"))
	data[idx] = 'Z'
	_ = os.WriteFile(path, data, 0o644)

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, trustedEnv(), path, nil,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	if g.ApproveStructured(openPR("feat/x")) {
		t.Fatal("broken chain must never auto-approve")
	}
	assertDenyReason(t, sink, "chain_broken")
}

func TestApproveStructuredRateExceeded(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	// 3 greens to clear the bar, plus 2 prior auto_approve events TODAY for the same
	// scope so the rate window (MaxPerDay=2) is already exhausted.
	entries := append(greenRun("open-pr", "feat/x", 3),
		logEntry{kind: "auto_approve", action: "open-pr", scope: "feat/x", passed: true},
		logEntry{kind: "auto_approve", action: "open-pr", scope: "feat/x", passed: true},
	)
	path := writeLog(t, dir, entries)

	human := &recHuman{reply: false}
	sink := &recSink{}
	// NOTE: countAutoApprovalsToday keys on the event's own Time (≈ now in the test
	// run). Use the real clock day so today matches the appended events.
	g := newGraded(human, trustedEnv(), path, nil,
		WithSink(sink), WithClock(fixedClock(time.Now().UTC())), WithRoot(dir))
	_ = now

	if g.ApproveStructured(openPR("feat/x")) {
		t.Fatal("rate-exceeded action must not auto-approve")
	}
	assertDenyReason(t, sink, "rate_exceeded")
}

func TestApproveStructuredOverDollarCeiling(t *testing.T) {
	dir := t.TempDir()
	path := writeLog(t, dir, greenRun("deploy", "staging", 3))

	env := Envelope{Classes: []ClassClause{{
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

	// Blast $10/day ceiling, $9 already spent today; this action's own $2 cost would push
	// the day to $11 > $10 ⇒ refused by the tighter blast fence (min(clause 25, blast 10)).
	blast := blastbudget.New()
	blast.SetAutoApprovalDollarCeiling(10)
	today := dayKey(time.Now().UTC())
	if err := blast.ChargeAutoApprovalDollars(context.Background(), today, 9); err != nil {
		t.Fatalf("setup charge: %v", err)
	}
	withPerActionCost(t, 2)

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, env, path, blast,
		WithSink(sink), WithClock(fixedClock(time.Now().UTC())), WithRoot(dir))

	// Evidence.SpentUSD (cumulative run ledger) is NOT this action's cost — it must not affect
	// the charge; only the action's own $2 cost is metered.
	deploy := policy.GateAction{Type: policy.Deploy, Branch: "staging", Evidence: &policy.GateEvidence{SpentUSD: 500}}
	if g.ApproveStructured(deploy) {
		t.Fatal("over-ceiling deploy must not auto-approve")
	}
	assertDenyReason(t, sink, "over_ceiling")
}

// A clause that declares a positive $/day ceiling but has NO blast meter wired
// (g.blast == nil, the default when -blast-radius is off) is REACHABLE and metered by the
// clause's OWN MaxDollarsDay — NOT blanket-denied "unmetered". This is the regression fix:
// the prior code failed closed on any positive cost under a nil meter, which (since
// build.go set Evidence.SpentUSD to the CUMULATIVE run ledger on every action) denied every
// auto-approval once the run had spent money. The clause ceiling now bounds the per-day
// auto-approved total in-process. This test proves the ceiling BITES on the nil-meter path
// when a per-action charge would exceed it, and is REACHABLE when it would not.
func TestApproveStructuredNilMeterClauseCeiling(t *testing.T) {
	dir := t.TempDir()
	path := writeLog(t, dir, greenRun("deploy", "staging", 3))

	env := Envelope{Classes: []ClassClause{{
		Type:          "deploy",
		AllowBranches: []string{"staging"},
		DenyBranches:  commonDeny,
		Environments:  []string{"staging"},
		MinSuccesses:  2,
		MinSample:     2,
		RecencyDays:   7,
		MaxPerDay:     5,
		MaxDollarsDay: 25, // positive ceiling, no blast meter — the clause alone must bound it
	}}}

	// Reachable: this action's own cost $10 <= the $25 clause ceiling ⇒ auto-approve, even
	// though the cumulative run ledger (Evidence.SpentUSD) is $500. The ledger is NOT charged.
	withPerActionCost(t, 10)
	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, env, path, nil, // nil meter — default -blast-radius off
		WithSink(sink), WithClock(fixedClock(time.Now().UTC())), WithRoot(dir))
	reachable := policy.GateAction{Type: policy.Deploy, Branch: "staging", Evidence: &policy.GateEvidence{SpentUSD: 500}}
	if !g.ApproveStructured(reachable) {
		t.Fatalf("an in-clause-budget deploy must auto-approve under a nil meter (last: %+v)", mustLast(t, sink).detail)
	}
	if human.called {
		t.Fatal("a reachable nil-meter auto-approval must not consult the human")
	}

	// Bites: a fresh approver whose action's own cost $26 > the $25 clause ceiling ⇒ deny
	// over_ceiling and delegate, WITHOUT any blast meter. Not "unmetered".
	withPerActionCost(t, 26)
	human2 := &recHuman{reply: false}
	sink2 := &recSink{}
	g2 := newGraded(human2, env, path, nil,
		WithSink(sink2), WithClock(fixedClock(time.Now().UTC())), WithRoot(dir))
	if g2.ApproveStructured(deployStaging()) {
		t.Fatal("an over-clause-ceiling deploy must not auto-approve under a nil meter")
	}
	if !human2.called {
		t.Fatal("an over-ceiling nil-meter action must delegate to the human")
	}
	assertDenyReason(t, sink2, "over_ceiling")
}

// SIF-T07: the self-improve auto-approval CLASS is its OWN double opt-in.
func TestSelfImproveGate(t *testing.T) {
	humanAsked := false
	human := func(string) bool { humanAsked = true; return false }
	sink := &recSink{}
	g := SelfImproveGate(human, sink)

	// Default-off: env unset ⇒ delegate to the human (and return the human's verdict).
	t.Setenv(EnvSelfImproveAutoApprove, "")
	if g("self-edit: docs/PERSONA.md") {
		t.Fatal("default-off must return the human's verdict (deny here)")
	}
	if !humanAsked {
		t.Fatal("default-off must consult the human")
	}
	if len(sink.events) != 0 {
		t.Fatal("default-off must emit no auto-approval event")
	}

	// Opted in: auto-approve WITHOUT consulting the human, and audit it.
	humanAsked = false
	t.Setenv(EnvSelfImproveAutoApprove, "1")
	if !g("self-edit: docs/PERSONA.md") {
		t.Fatal("opted-in must auto-approve the earned self-edit")
	}
	if humanAsked {
		t.Fatal("opted-in must NOT consult the human")
	}
	ev, ok := sink.last()
	if !ok || ev.kind != "auto_approve_selfimprove" {
		t.Fatalf("opted-in must emit an audited auto_approve_selfimprove event, got %+v", sink.events)
	}
}

// SIF-T07 truthiness: only an EXPLICIT "1" enables the self-improve auto-approval,
// mirroring the kill-switch. A "0"/"false"/empty value must NOT enable it — otherwise
// an operator setting `=0` to DISABLE it would have inverted the intent and ENABLED a
// free-text self-edit auto-approval.
func TestSelfImproveGateTruthiness(t *testing.T) {
	for _, tc := range []struct {
		val      string
		autoOK   bool // true ⇒ auto-approves (must not consult human); false ⇒ delegates
		askHuman bool // whether the human must be consulted
	}{
		{val: "", autoOK: false, askHuman: true},
		{val: "0", autoOK: false, askHuman: true},
		{val: "false", autoOK: false, askHuman: true},
		{val: "1", autoOK: true, askHuman: false},
	} {
		t.Run("val="+tc.val, func(t *testing.T) {
			humanAsked := false
			human := func(string) bool { humanAsked = true; return false }
			sink := &recSink{}
			g := SelfImproveGate(human, sink)

			t.Setenv(EnvSelfImproveAutoApprove, tc.val)
			got := g("self-edit: docs/PERSONA.md")

			if got != tc.autoOK {
				t.Fatalf("val=%q: gate returned %v, want %v", tc.val, got, tc.autoOK)
			}
			if humanAsked != tc.askHuman {
				t.Fatalf("val=%q: humanAsked=%v, want %v", tc.val, humanAsked, tc.askHuman)
			}
			emitted := len(sink.events) != 0
			if emitted != tc.autoOK {
				t.Fatalf("val=%q: emitted auto_approve event=%v, want %v", tc.val, emitted, tc.autoOK)
			}
		})
	}
}

// BR-T04: every auto-approval consumes one slot of the SHARED blast meter's
// irreversible axis (composition law min(P5, blast)); a breach falls through to the
// human, regardless of how much earned trust the clause has.
func TestApproveStructuredBlastIrreversibleFence(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 3))

	blast := blastbudget.New()
	blast.SetIrreversibleCeiling(1) // exactly one auto-approval allowed this run

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, trustedEnv(), path, blast,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	// First clears the trust bar AND fits the irreversible budget.
	if !g.ApproveStructured(openPR("feat/x")) {
		t.Fatal("first auto-approval must pass within the blast budget")
	}
	if human.called {
		t.Fatal("a pass must not consult the human")
	}
	// Second is identically trusted but the irreversible axis is now exhausted, so it
	// is denied and delegated — earned trust cannot exceed the hard runtime fence.
	if g.ApproveStructured(openPR("feat/x")) {
		t.Fatal("second auto-approval must be denied by the blast irreversible fence")
	}
	assertDenyReason(t, sink, "blast_radius")
	if !human.called {
		t.Fatal("a blast-radius breach must fall through to the human")
	}
}

// BR-T04: the irreversible slot is taken BEFORE the per-day $ charge; if the dollar
// ceiling refuses, the slot is rolled back so a denied action consumes nothing.
func TestApproveStructuredDollarBreachRollsBackIrreversible(t *testing.T) {
	dir := t.TempDir()
	path := writeLog(t, dir, greenRun("deploy", "staging", 3))

	env := Envelope{Classes: []ClassClause{{
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

	blast := blastbudget.New()
	blast.SetIrreversibleCeiling(5)        // irreversible has plenty of room
	blast.SetAutoApprovalDollarCeiling(10) // but the $ ceiling ($10) is exhausted below
	today := dayKey(time.Now().UTC())
	if err := blast.ChargeAutoApprovalDollars(context.Background(), today, 10); err != nil {
		t.Fatalf("setup charge to ceiling: %v", err)
	}
	withPerActionCost(t, 1) // any positive charge now breaches the exhausted $10 ceiling

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, env, path, blast,
		WithSink(sink), WithClock(fixedClock(time.Now().UTC())), WithRoot(dir))

	// This action's own $1 cost breaches the exhausted $10 day ceiling ⇒ the $ charge
	// refuses AFTER the irreversible slot was taken, which must then be rolled back.
	deploy := policy.GateAction{Type: policy.Deploy, Branch: "staging", Evidence: &policy.GateEvidence{SpentUSD: 25}}
	if g.ApproveStructured(deploy) {
		t.Fatal("over-$-ceiling deploy must not auto-approve")
	}
	assertDenyReason(t, sink, "over_ceiling")
	// The slot taken before the $ charge must be released — a denied action is free.
	if u := blast.Used(dayKey(time.Now().UTC())); u.Irreversible != 0 {
		t.Fatalf("denied auto-approval must consume no irreversible slot, got %d", u.Irreversible)
	}
}

func TestApproveStructuredDeployProdAlwaysDenied(t *testing.T) {
	dir := t.TempDir()
	path := writeLog(t, dir, greenRun("deploy", "prod", 3))
	env := Envelope{Classes: []ClassClause{{
		Type:          "deploy",
		AllowBranches: []string{"*"},
		Environments:  []string{"prod"}, // even if operator lists prod, prod* wins
		MinSuccesses:  2,
		MinSample:     2,
		RecencyDays:   7,
		MaxPerDay:     5,
	}}}
	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, env, path, nil,
		WithSink(sink), WithClock(fixedClock(time.Now().UTC())), WithRoot(dir))

	if g.ApproveStructured(policy.GateAction{Type: policy.Deploy, Branch: "prod"}) {
		t.Fatal("prod deploy must never auto-approve")
	}
	assertDenyReason(t, sink, "out_of_scope")
}

// Free-text Approve must NEVER auto-approve — it delegates straight to the human.
func TestApproveFreeTextDelegates(t *testing.T) {
	dir := t.TempDir()
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 3))
	human := &recHuman{reply: false}
	g := newGraded(human, trustedEnv(), path, nil, WithRoot(dir))

	if g.Approve("git push --force") {
		t.Fatal("free-text Approve must never auto-approve")
	}
	if !human.called {
		t.Fatal("free-text Approve must delegate to the human")
	}
}

func assertDenyReason(t *testing.T, sink *recSink, want string) {
	t.Helper()
	ev, ok := sink.last()
	if !ok {
		t.Fatalf("expected an auto_deny event, got none")
	}
	if ev.kind != "auto_deny" {
		t.Fatalf("expected auto_deny, got %q", ev.kind)
	}
	if got, _ := ev.detail["reason"].(string); got != want {
		t.Fatalf("auto_deny reason = %q, want %q (detail=%+v)", got, want, ev.detail)
	}
}

// --- MaybeWrap default-off ------------------------------------------------

func TestMaybeWrapReturnsHumanUnchanged(t *testing.T) {
	human := &recHuman{}
	// nil envelope
	if got := MaybeWrap(human, nil, "x.log", nil); got != policy.Approver(human) {
		t.Fatal("nil envelope must return the human approver unchanged (same value)")
	}
	// empty envelope
	if got := MaybeWrap(human, &Envelope{}, "x.log", nil); got != policy.Approver(human) {
		t.Fatal("empty envelope must return the human approver unchanged (same value)")
	}
	// a real envelope constructs a GradedApprover (NOT the human)
	got := MaybeWrap(human, &Envelope{Classes: trustedEnv().Classes}, "x.log", nil)
	if got == policy.Approver(human) {
		t.Fatal("a configured envelope must construct a GradedApprover, not return the human")
	}
	if _, ok := got.(*GradedApprover); !ok {
		t.Fatalf("expected *GradedApprover, got %T", got)
	}
}

// TestCountAutoApprovalsToday: the per-day rate counter folds only `auto_approve` events
// for (action, scope FAMILY) on today's day — never boundary_outcome / auto_deny, never
// another action, never another family, never another day. (M11: the gate's MaxPerDay
// rate window depends on this counting only the right events.)
//
// The window keys on the FAMILY (feat/x and feat/y are both feat/*), not the exact
// branch. Every live scope is unique per run, so an exact-branch window would open fresh
// on every decision and MaxPerDay would never bind at all.
func TestCountAutoApprovalsToday(t *testing.T) {
	dir := t.TempDir()
	path := writeLog(t, dir, []logEntry{
		{kind: "auto_approve", action: "open-pr", scope: "feat/x", passed: true},
		{kind: "auto_approve", action: "open-pr", scope: "feat/x", passed: true},
		{kind: "auto_approve", action: "open-pr", scope: "feat/y", passed: true},         // SAME family (feat/*)
		{kind: "auto_approve", action: "open-pr", scope: "task/z", passed: true},         // different family
		{kind: "auto_approve", action: "promote-to-base", scope: "feat/x", passed: true}, // different action
		{kind: "boundary_outcome", action: "open-pr", scope: "feat/x", passed: true},     // wrong kind
		{kind: "auto_deny", action: "open-pr", scope: "feat/x"},                          // wrong kind
	})
	today := dayKey(time.Now().UTC())

	n, err := countAutoApprovalsToday(path, "open-pr", "feat/x", today)
	if err != nil || n != 3 {
		t.Fatalf("count = %d, %v; want 3 (today's open-pr auto_approve over the feat/* family)", n, err)
	}
	// A different family keeps its own window — the cap is per (action, family).
	if n, err := countAutoApprovalsToday(path, "open-pr", "task/z", today); err != nil || n != 1 {
		t.Fatalf("count for task/* = %d, %v; want 1 (families do not share a window)", n, err)
	}
	if n, _ := countAutoApprovalsToday(path, "open-pr", "feat/x", "1999-01-01"); n != 0 {
		t.Errorf("a non-today day must count 0, got %d", n)
	}
	if n, err := countAutoApprovalsToday(dir+"/missing.log", "open-pr", "feat/x", today); err != nil || n != 0 {
		t.Errorf("a missing log = (0, nil), got (%d, %v)", n, err)
	}
}

// safeHuman / safeSink are mutex-guarded test doubles for the concurrency test below
// (recHuman/recSink touch fields without a lock, which -race would flag when the human
// fall-through runs on two goroutines at once).
type safeHuman struct {
	mu     sync.Mutex
	called int
	reply  bool
}

func (h *safeHuman) Approve(string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.called++
	return h.reply
}

func (h *safeHuman) calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.called
}

type safeSink struct {
	mu     sync.Mutex
	kinds  map[string]int
	denies map[string]int // reason -> count
}

func newSafeSink() *safeSink {
	return &safeSink{kinds: map[string]int{}, denies: map[string]int{}}
}

func (s *safeSink) Emit(kind string, detail map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kinds[kind]++
	if kind == "auto_deny" {
		if r, _ := detail["reason"].(string); r != "" {
			s.denies[r]++
		}
	}
}

func (s *safeSink) count(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kinds[kind]
}

func (s *safeSink) denyReason(r string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.denies[r]
}

// TestApproveStructuredRateCapConcurrent proves the per-day rate cap is ATOMIC: two
// concurrent ApproveStructured calls for the SAME (type,scope) with MaxPerDay=1 result
// in EXACTLY ONE auto-approval; the other falls through to the human on the rate gate.
// Without the in-process check-and-increment authority both would read rate=0 from the
// read-only log scan, both pass, and the cap is exceeded (the TOCTOU this test guards).
// Run under -race to also catch unsynchronized access to the counter.
func TestApproveStructuredRateCapConcurrent(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC() // real day so the trust-log greens count as recent
	// Clear the bar (MinSuccesses/MinSample=2) and leave the rate window empty.
	path := writeLog(t, dir, greenRun("open-pr", "feat/x", 3))

	env := Envelope{Classes: []ClassClause{{
		Type:          "open-pr",
		AllowBranches: []string{"feat/*"},
		DenyBranches:  commonDeny,
		MinSuccesses:  2,
		MinSample:     2,
		RecencyDays:   7,
		MaxPerDay:     1, // exactly one auto-approval allowed today
	}}}

	human := &safeHuman{reply: false}
	sink := newSafeSink()
	g := newGraded(human, env, path, nil,
		WithSink(sink), WithClock(fixedClock(now)), WithRoot(dir))

	const n = 2
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		approvals int
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release both goroutines together to maximize overlap
			if g.ApproveStructured(openPR("feat/x")) {
				mu.Lock()
				approvals++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if approvals != 1 {
		t.Fatalf("concurrent decisions auto-approved %d times, want exactly 1 (rate cap MaxPerDay=1)", approvals)
	}
	if got := sink.count("auto_approve"); got != 1 {
		t.Fatalf("emitted %d auto_approve events, want 1", got)
	}
	// The loser must fall through to the human via the rate gate.
	if got := human.calls(); got != n-1 {
		t.Fatalf("human consulted %d times, want %d (the non-approved decision)", got, n-1)
	}
	if got := sink.denyReason("rate_exceeded"); got != n-1 {
		t.Fatalf("rate_exceeded denies = %d, want %d", got, n-1)
	}
}

// recStructuredHuman is a human approver that ALSO implements the structured
// seam, recording the full action it received — the fall-through must forward
// the whole GateAction (Evidence intact), not the flattened Describe() string.
type recStructuredHuman struct {
	recHuman
	structCalled bool
	got          policy.GateAction
}

func (h *recStructuredHuman) ApproveStructured(a policy.GateAction) bool {
	h.structCalled = true
	h.got = a
	return h.reply
}

// TestFallThroughForwardsEvidenceToStructuredHuman pins the graded wrapper's
// fall-through: a structured human receives the FULL action with its decision
// Evidence, while a plain human keeps the exact flat Describe() path (covered
// by the other fall-through tests above).
func TestFallThroughForwardsEvidenceToStructuredHuman(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	path := writeLog(t, dir, greenRun("push", "feat/x", 3))

	human := &recStructuredHuman{recHuman: recHuman{reply: true}}
	g := newGraded(human, trustedEnv(), path, nil, // env has open-pr only ⇒ Push falls through
		WithSink(&recSink{}), WithClock(fixedClock(now)), WithRoot(dir))

	ev := &policy.GateEvidence{Diffstat: "1 file changed"}
	if !g.ApproveStructured(policy.GateAction{Type: policy.Push, Branch: "feat/x", Evidence: ev}) {
		t.Fatal("structured human approved ⇒ fall-through must return true")
	}
	if !human.structCalled {
		t.Fatal("fall-through must use the human's structured seam when available")
	}
	if human.called {
		t.Fatal("structured forward must not ALSO hit the flat Approve path")
	}
	if human.got.Evidence != ev {
		t.Fatal("fall-through must forward the action's Evidence intact")
	}
}
