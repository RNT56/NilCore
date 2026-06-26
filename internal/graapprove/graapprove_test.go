package graapprove

import (
	"context"
	"os"
	"path/filepath"
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
	kind   string // default "boundary_outcome"
	action string
	scope  string
	passed bool
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

	// blast budget already at its $ ceiling for today ⇒ the charge is refused.
	blast := blastbudget.New()
	blast.SetAutoApprovalDollarCeiling(10) // smaller than the $25 charge
	today := dayKey(time.Now().UTC())
	_ = blast.ChargeAutoApprovalDollars(context.Background(), today, 0)

	human := &recHuman{reply: false}
	sink := &recSink{}
	g := newGraded(human, env, path, blast,
		WithSink(sink), WithClock(fixedClock(time.Now().UTC())), WithRoot(dir))

	deploy := policy.GateAction{Type: policy.Deploy, Branch: "staging"}
	if g.ApproveStructured(deploy) {
		t.Fatal("over-ceiling deploy must not auto-approve")
	}
	assertDenyReason(t, sink, "over_ceiling")
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
