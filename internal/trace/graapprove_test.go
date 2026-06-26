package trace

import (
	"os"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
)

// graapprove_test.go covers the GAA-T08 trace half: the trace recognizes the
// three graduated-auto-approval audit events (auto_approve, auto_deny,
// boundary_outcome) as causal nodes with harness-authored Title + Why, surfaces
// ONLY metadata-shaped evidence (never a secret or a model body, I7), and marks
// them untrusted along with every other node over a broken chain (I5).

// graApproveRun is a run that exercises all three GAA event kinds with the exact
// Detail shapes the graapprove/project emitters write: a verifier-green
// boundary_outcome that earns trust, the auto_approve it enables (with the full
// nested evidence object), and an auto_deny that falls back to the human.
func graApproveRun() []eventlog.Event {
	return []eventlog.Event{
		{Task: "T", Kind: "task_start", Detail: map[string]any{"goal": "ship the promote"}},
		// The verifier verdict on the integration tip that feeds earned trust.
		{Task: "T", Kind: "boundary_outcome", Detail: map[string]any{
			"action": "promote-to-base", "scope": "task/widget", "passed": true, "chain": true,
		}},
		// The graded approval the earned trust enables — full evidence object,
		// with the nested bar/rate/dollars wrappers the emitter writes.
		{Task: "T", Kind: "auto_approve", Detail: map[string]any{
			"action": "promote-to-base", "scope": "task/widget",
			"green": float64(5), "total": float64(5),
			"last_green": "2026-06-26T00:00:00Z",
			"bar":        map[string]any{"min_successes": float64(3), "min_sample": float64(3), "recency_days": float64(30)},
			"rate":       map[string]any{"count": float64(0), "max_per_day": float64(2)},
			"dollars":    map[string]any{"charged": float64(0), "max_dollars_day": float64(0)},
			"chain_ok":   true,
		}},
		// A graded decision that did not clear — falls through to the human.
		{Task: "T", Kind: "auto_deny", Detail: map[string]any{
			"reason": "below_bar", "action": "promote-to-base", "scope": "task/other",
			"green": float64(1), "total": float64(2),
			"min_successes": float64(3), "min_sample": float64(3),
			"recency_days": float64(30), "recent_ok": true,
		}},
	}
}

func TestBuild_AutoApproveNode(t *testing.T) {
	path := writeLog(t, graApproveRun())
	tr, err := Build(path, "T")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	steps := stepsOfKind(tr.Steps, "auto_approve")
	if len(steps) != 1 {
		t.Fatalf("auto_approve nodes = %d, want 1: %v", len(steps), kinds(tr.Steps))
	}
	s := steps[0]
	if !contains(s.Title, "auto-approved promote-to-base") || !contains(s.Title, "task/widget") {
		t.Fatalf("auto_approve title = %q, want it to name the action and scope", s.Title)
	}
	if !contains(s.Why, "earned trust") || !contains(s.Why, "envelope") {
		t.Fatalf("auto_approve Why = %q, want the earned-trust-met-the-bar story", s.Why)
	}
	// The numeric trust evidence and the flattened bar fields must surface.
	for _, k := range []string{"action", "scope", "green", "total", "min_successes", "min_sample", "recency_days", "max_per_day"} {
		if _, ok := s.Detail[k]; !ok {
			t.Fatalf("auto_approve Detail missing evidence key %q: %v", k, s.Detail)
		}
	}
	if s.Detail["green"] != "5" || s.Detail["min_successes"] != "3" || s.Detail["max_per_day"] != "2" {
		t.Fatalf("auto_approve evidence garbled: %v", s.Detail)
	}
	// The nested wrapper objects themselves are flattened away — never surfaced as
	// a blob value.
	for _, k := range []string{"bar", "rate", "dollars"} {
		if v, ok := s.Detail[k]; ok {
			t.Fatalf("auto_approve Detail surfaced nested wrapper %q as a blob: %q", k, v)
		}
	}
}

func TestBuild_AutoDenyNode(t *testing.T) {
	path := writeLog(t, graApproveRun())
	tr, _ := Build(path, "T")
	steps := stepsOfKind(tr.Steps, "auto_deny")
	if len(steps) != 1 {
		t.Fatalf("auto_deny nodes = %d, want 1", len(steps))
	}
	s := steps[0]
	if !contains(s.Title, "promote-to-base") || !contains(s.Title, "fell to the human") {
		t.Fatalf("auto_deny title = %q, want it to fall to the human", s.Title)
	}
	if !contains(s.Why, "below_bar") {
		t.Fatalf("auto_deny Why = %q, want it to name the reason", s.Why)
	}
	if s.Detail["reason"] != "below_bar" || s.Detail["scope"] != "task/other" {
		t.Fatalf("auto_deny evidence garbled: %v", s.Detail)
	}
}

func TestBuild_BoundaryOutcomeNode(t *testing.T) {
	path := writeLog(t, graApproveRun())
	tr, _ := Build(path, "T")
	steps := stepsOfKind(tr.Steps, "boundary_outcome")
	if len(steps) != 1 {
		t.Fatalf("boundary_outcome nodes = %d, want 1", len(steps))
	}
	s := steps[0]
	if !contains(s.Title, "verifier-green promote-to-base boundary") || !contains(s.Title, "task/widget") {
		t.Fatalf("boundary_outcome title = %q, want a verifier-green boundary on the scope", s.Title)
	}
	if !contains(s.Why, "verifier") || !contains(s.Why, "earns trust") {
		t.Fatalf("boundary_outcome Why = %q, want the verifier-earns-trust story", s.Why)
	}
	if s.Detail["passed"] != "true" || s.Detail["action"] != "promote-to-base" {
		t.Fatalf("boundary_outcome evidence garbled: %v", s.Detail)
	}
}

// TestBuild_BoundaryOutcomeFailedNode covers the non-passing verifier verdict —
// it must read as no-trust-earned, never as a win.
func TestBuild_BoundaryOutcomeFailedNode(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		{Task: "T", Kind: "task_start", Detail: map[string]any{"goal": "g"}},
		{Task: "T", Kind: "boundary_outcome", Detail: map[string]any{
			"action": "deploy", "scope": "staging", "passed": false, "chain": true,
		}},
	})
	tr, _ := Build(path, "T")
	steps := stepsOfKind(tr.Steps, "boundary_outcome")
	if len(steps) != 1 {
		t.Fatalf("boundary_outcome nodes = %d, want 1", len(steps))
	}
	if contains(steps[0].Title, "verifier-green") {
		t.Fatalf("failed boundary_outcome title = %q, must not claim verifier-green", steps[0].Title)
	}
	if !contains(steps[0].Why, "no trust") {
		t.Fatalf("failed boundary_outcome Why = %q, want the no-trust-earned story", steps[0].Why)
	}
}

// TestBuild_GAANodesEvidenceIsMetadataOnly is the I7 teeth for this task: a
// secret-shaped body smuggled into the GAA events under non-allowlisted keys (and
// inside a nested wrapper) must NEVER reach a Step or the render. Only metadata
// scalars survive.
func TestBuild_GAANodesEvidenceIsMetadataOnly(t *testing.T) {
	const secretPhrase = "sk-live-DEADBEEF-LEAK-THE-CREDENTIAL"
	path := writeLog(t, []eventlog.Event{
		{Task: "T", Kind: "task_start", Detail: map[string]any{"goal": "g"}},
		{Task: "T", Kind: "auto_approve", Detail: map[string]any{
			"action": "promote-to-base", "scope": "task/x",
			"green": float64(3), "total": float64(3),
			// Non-allowlisted body fields, top-level and nested.
			"prompt": secretPhrase,
			"secret": secretPhrase,
			"bar":    map[string]any{"min_successes": float64(3), "leak": secretPhrase},
		}},
	})
	tr, _ := Build(path, "T")
	out := Render(tr, plainStyle)
	if strings.Contains(out, secretPhrase) {
		t.Fatalf("secret-shaped body leaked into the GAA render — I7 violation:\n%s", out)
	}
	assertNoPhrase(t, tr.Steps, secretPhrase)
	// The allowlisted evidence still came through, including the flattened bar field.
	s := stepsOfKind(tr.Steps, "auto_approve")[0]
	if s.Detail["green"] != "3" || s.Detail["min_successes"] != "3" {
		t.Fatalf("metadata evidence dropped while filtering the body: %v", s.Detail)
	}
}

// TestBuild_GAANodesUntrustedOverBrokenChain proves the GAA audit nodes obey the
// same fail-closed discipline as every other node: a tampered log marks them all
// untrusted (I5).
func TestBuild_GAANodesUntrustedOverBrokenChain(t *testing.T) {
	path := writeLog(t, graApproveRun())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := replaceFirst(string(data), "ship the promote", "ship the PROMOTE")
	if corrupt == string(data) {
		t.Fatal("test setup: tamper string not found")
	}
	if err := os.WriteFile(path, []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}
	if eventlog.Verify(path) == nil {
		t.Fatal("test setup: tampered log still verifies")
	}

	tr, err := Build(path, "T")
	if err != nil {
		t.Fatalf("Build over a broken chain should still return a structural trace: %v", err)
	}
	if tr.ChainVerified {
		t.Fatal("ChainVerified = true over a tampered log")
	}
	for _, kind := range []string{"auto_approve", "auto_deny", "boundary_outcome"} {
		steps := stepsOfKind(tr.Steps, kind)
		if len(steps) == 0 {
			t.Fatalf("%s node missing over a broken chain (structure must still show)", kind)
		}
		for _, s := range steps {
			if !s.Untrusted {
				t.Fatalf("%s node #%d not marked untrusted over a broken chain", kind, s.Seq)
			}
		}
	}
}

// TestRender_NoGAAEventsUnchanged is the default-path golden: a run that emits
// NO graduated-auto-approval events renders exactly the canonical realistic-run
// output, with none of the new GAA strings or evidence keys leaking in. The GAA
// surfacing is purely additive — it must be byte-identical and invisible when
// unwired.
func TestRender_NoGAAEventsUnchanged(t *testing.T) {
	path := writeLog(t, realisticRun())
	tr, _ := Build(path, "T")
	out := Render(tr, plainStyle)

	const golden = `why: T
  goal: make the widget green
  ✓ chain verified — this trace is trustworthy
  verdict: integrated — the verified branch was merged

  ▸ #0 · task started — goal: make the widget green
      · base_repo=/repo
  ▸ #1 · model turn (tool_use) [native]
      · out_tokens=[redacted]
      · step=0
      · stop=tool_use
    ▸ #2 · ran tool: edit [native]
        · tool=edit
  ✗ #3 · verify FAILED [native] — the project's checks did not pass — this gates the run, not the backend's self-report
      · passed=false
  ⚑ #4 · consulted the advisor [native] — to recover after 1 failed verify — asked the advisor for a way forward
      · calls=1
  ▸ #5 · model turn (tool_use) [native] — re-planning after the failed verify, with the advisor's guidance
      · out_tokens=[redacted]
      · step=1
      · stop=tool_use
    ▸ #6 · ran tool: edit [native]
        · tool=edit
  ✓ #7 · verify PASSED [native] — green after 1 failed attempt
      · passed=true
  ✓ #8 · human gate: approved — merge task/T is irreversible (class irreversible), so it required human sign-off
      · action=merge task/T
      · allowed=true
      · class=irreversible
  ✓ #9 · integrated branch task/T — the branch verified green, so its work was merged in
      · branch=task/T
      · pre_sha=aaa
      · sha=bbb

events:
     1 advisor_consult
     1 gate
     1 integration_merge
     2 model_call
     1 task_start
     2 tool_exec
     2 verify
`
	if out != golden {
		t.Fatalf("default-path render drifted from the golden (additive change must be byte-identical when unwired):\n--- got ---\n%s\n--- want ---\n%s", out, golden)
	}
	// Belt-and-braces: no GAA-specific string or evidence key can appear in a run
	// that emits no GAA events.
	for _, banned := range []string{"auto-approved", "fell to the human", "boundary", "earned trust", "min_successes", "max_per_day"} {
		if strings.Contains(out, banned) {
			t.Fatalf("GAA string %q leaked into a GAA-free render:\n%s", banned, out)
		}
	}
}

// TestRender_GAANodesRenderTitledRows is the render-side smoke test: the three
// kinds appear in the rendered tree with their harness-authored titles.
func TestRender_GAANodesRenderTitledRows(t *testing.T) {
	path := writeLog(t, graApproveRun())
	tr, _ := Build(path, "T")
	out := Render(tr, plainStyle)
	for _, want := range []string{
		"auto-approved promote-to-base on task/widget",
		"fell to the human",
		"verifier-green promote-to-base boundary on task/widget",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing GAA title %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("off-Style GAA render contains ANSI escapes:\n%q", out)
	}
}
