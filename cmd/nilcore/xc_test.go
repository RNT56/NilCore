package main

// xc_test.go holds the Phase-16 CROSS-CUTTING guarantees (docs/ROADMAP-CLOSED-LOOP.md
// §7) — the invariants no single pillar owns, asserted at the integration boundary:
//
//   - XC-T02: no single flag/env transitively enables auto_approve.
//   - XC-T03: the graduated-approval policy (envelope/trust tallies) never links into
//     the model-facing loop, so it can never be fed to the model (I3).
//   - XC-T06: objective CRUD never links into the model TOOL surface, so a model can
//     never enqueue/edit its own standing objectives (I7-adjacent / operator-only).
//
// (XC-T01 — blastbudget is the sole $/rate meter — ships in internal/graapprove's
// sole_meter_test.go; XC-T04 rebuild-on-boot and XC-T05 revocation surface have their
// own tests.)

import (
	"io"
	"os/exec"
	"strings"
	"testing"

	"nilcore/internal/blastbudget"
	"nilcore/internal/graapprove"
	"nilcore/internal/onboard"
	"nilcore/internal/policy"
)

// TestXC02_NoTransitiveOptIn proves no single closed-loop relaxation reaches auto-
// approval without its OWN explicit gate: with every other flag/env on AND a real blast
// budget, an absent envelope still yields the human approver UNCHANGED.
func TestXC02_NoTransitiveOptIn(t *testing.T) {
	human := policy.NewConsoleApprover(strings.NewReader(""), io.Discard)

	// Turn on every OTHER closed-loop relaxation — none may open the auto-approval door.
	for _, env := range []string{
		"NILCORE_EXPERIENCE", "NILCORE_TRUST_DEFAULT", "NILCORE_VCACHE",
		"NILCORE_LESSONS", "NILCORE_FLYWHEEL", "NILCORE_AUTONOMY",
	} {
		t.Setenv(env, "1")
	}
	// A real blast budget (as if `-blast-radius standard`) — still no auto-approval.
	blast := blastbudget.New()
	blast.SetIrreversibleCeiling(5)

	// No envelope ⇒ wrapAutoApprove returns the human approver UNCHANGED: the graded
	// approver is never constructed, so no auto_approve is reachable. Auto-approval
	// requires its OWN explicit opt-in (the operator envelope), never another flag.
	if got := wrapAutoApprove(human, onboard.Config{}, "", "x.jsonl", nil, blast); got != policy.Approver(human) {
		t.Fatal("no single flag/env may transitively enable auto-approval (XC-T02)")
	}

	// The self-improve auto-approval class needs its OWN env, DISTINCT from enabling the
	// flywheel — turning NILCORE_FLYWHEEL on (above) must not auto-merge self-edits.
	t.Setenv(graapprove.EnvSelfImproveAutoApprove, "")
	gate := graapprove.SelfImproveGate(func(string) bool { return false }, nil)
	if gate("self-edit: docs/PERSONA.md") {
		t.Fatal("self-improve auto-merge needs NILCORE_SELFIMPROVE_AUTOAPPROVE, not the flywheel env (XC-T02)")
	}
}

// TestXC03_ModelNeverSeesPolicy proves the graduated-approval policy (the envelope +
// trust tallies in internal/graapprove) never LINKS into the model-facing loop
// (internal/backend, internal/model), so it can never be fed into a prompt or tool (I3).
func TestXC03_ModelNeverSeesPolicy(t *testing.T) {
	for _, pkg := range []string{"nilcore/internal/backend", "nilcore/internal/model"} {
		if depSet(t, pkg)["nilcore/internal/graapprove"] {
			t.Errorf("%s must not link the graduated-approval policy (graapprove) — it must never reach the model (I3, XC-T03)", pkg)
		}
	}
}

// TestXC06_ObjectivesUnreachableFromModelTools proves objective CRUD never links into
// the model TOOL surface (internal/tools): a model can only call REGISTERED tools, and
// objective is not one — so it can never enqueue/edit its own standing objectives
// (operator-only by construction, XC-T06). (The store links objective for typing, which
// is why the assertion is on the tool surface, not the whole binary.)
func TestXC06_ObjectivesUnreachableFromModelTools(t *testing.T) {
	if depSet(t, "nilcore/internal/tools")["nilcore/internal/objective"] {
		t.Error("internal/tools (the model tool surface) must not link internal/objective — objective CRUD is operator-only (XC-T06)")
	}
}

// depSet returns the transitive import set of pkg via `go list -deps`.
func depSet(t *testing.T, pkg string) map[string]bool {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
	}
	set := map[string]bool{}
	for _, d := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		set[strings.TrimSpace(d)] = true
	}
	return set
}
