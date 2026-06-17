package super

import (
	"context"
	"strings"
	"sync"
	"testing"

	"nilcore/internal/model"
	"nilcore/internal/roster"
	"nilcore/internal/spawn"
)

// TestContinueFromBuildsOnPriorAttempt is the headline: a retry with continue_from set
// to a PRIOR failed/incomplete subagent is cut from that attempt's preserved branch
// (so the new worker builds on the partial work), the failed attempt is NEVER
// integrated or made the tip (I2), and only the verified retry lands. Driven through
// the real Run loop in BOTH dispatch modes (serial and concurrent).
func TestContinueFromBuildsOnPriorAttempt(t *testing.T) {
	for _, concurrency := range []int{1, 4} {
		concurrency := concurrency
		name := "serial"
		if concurrency > 1 {
			name = "concurrent"
		}
		t.Run(name, func(t *testing.T) {
			m := &scriptModel{responses: []model.Response{
				textResp(toolUse("u1", "spawn_subagent", SubagentSpec{ID: "super.t1", Role: roster.RoleImplementer, Goal: "first attempt"})),
				textResp(toolUse("u2", "spawn_subagent", SubagentSpec{ID: "super.t1b", Role: roster.RoleImplementer, Goal: "finish it", ContinueFrom: "super.t1"})),
				textResp(toolUse("u3", "integrate", map[string]any{})),
				textResp(toolUse("u4", "finish", map[string]string{"summary": "done"})),
			}}
			s := baseSup(m, passVerifier{})
			s.Concurrency = concurrency

			var mu sync.Mutex
			bases := map[string]string{}
			s.Spawn = func(_ context.Context, spec SubagentSpec) spawn.Result {
				mu.Lock()
				bases[spec.ID] = spec.BaseRef
				mu.Unlock()
				if spec.ID == "super.t1" {
					// The first attempt fails, but the wiring site preserved its WIP on its
					// branch (preserveFailedAttempt) — so a failed Result now carries a Branch.
					return spawn.Result{ID: spec.ID, Passed: false, Branch: "task/super.t1", State: spawn.StateFailed}
				}
				return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID, State: spawn.StatePassed}
			}
			var order []string
			s.Integrate = noopIntegrate("integrate/tip", &order)

			out, err := s.Run(context.Background(), "goal")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			// The retry was cut from the prior FAILED attempt's preserved branch.
			if bases["super.t1b"] != "task/super.t1" {
				t.Errorf("continue_from did not re-base the retry: BaseRef=%q, want task/super.t1", bases["super.t1b"])
			}
			// I2: a failed attempt's branch must NEVER become the convergence tip.
			if out.Branch == "task/super.t1" {
				t.Errorf("a failed attempt's branch became the tip (I2 violated): %q", out.Branch)
			}
			// Only the verified retry integrates; the failed t1 is excluded by mergeOrder.
			if len(order) != 1 || order[0] != "super.t1b" {
				t.Errorf("integrate order = %v, want only [super.t1b] (failed t1 excluded)", order)
			}
		})
	}
}

// TestContinueFromValidationAndResolution covers the gate + resolver in isolation:
// continue_from must name a COMPLETED prior subagent; it resolves to that attempt's
// branch; and a prior with no branch (it changed nothing) degrades cleanly to "".
func TestContinueFromValidationAndResolution(t *testing.T) {
	s := &Supervisor{}
	st := &runState{handles: map[string]*Handle{
		"t1": {Spec: SubagentSpec{ID: "t1"}, Done: true, Result: spawn.Result{Passed: false, Branch: "task/t1"}}, // failed, WIP preserved
		"t2": {Spec: SubagentSpec{ID: "t2"}, Done: true, Result: spawn.Result{Passed: false, Branch: ""}},        // failed, nothing committed
		"t3": {Spec: SubagentSpec{ID: "t3"}, Done: false},                                                        // still running
	}}

	rails := []struct {
		cf      string
		wantErr bool
	}{
		{"t1", false}, // valid: a completed prior attempt
		{"nope", true},
		{"t3", true}, // not yet completed
		{"", false},  // omitted: default fresh start
	}
	for _, c := range rails {
		reason, denial := s.checkSpawnRails(st, SubagentSpec{ID: "new", Role: roster.RoleImplementer, Goal: "g", ContinueFrom: c.cf})
		if (reason != "") != c.wantErr {
			t.Errorf("checkSpawnRails(continue_from=%q) reason=%q, wantErr=%v", c.cf, reason, c.wantErr)
		}
		if reason != "" && denial {
			t.Errorf("continue_from=%q should be an input error, not a rail denial", c.cf)
		}
	}

	if got := s.continueBase(st, SubagentSpec{ID: "new", ContinueFrom: "t1"}); got != "task/t1" {
		t.Errorf("continueBase(t1) = %q, want task/t1", got)
	}
	if got := s.continueBase(st, SubagentSpec{ID: "new", ContinueFrom: "t2"}); got != "" {
		t.Errorf("continueBase(t2 with empty branch) = %q, want \"\" (degrade to base HEAD)", got)
	}
}

// TestContinueFromTakesPrecedenceOverDeps: a spec with BOTH depends_on and
// continue_from is cut from the prior attempt's branch (which already contains the
// deps' work), not re-resolved against the deps.
func TestContinueFromTakesPrecedenceOverDeps(t *testing.T) {
	s := &Supervisor{}
	st := &runState{handles: map[string]*Handle{
		"dep":   {Spec: SubagentSpec{ID: "dep"}, Done: true, Result: spawn.Result{Passed: true, Branch: "task/dep"}},
		"prior": {Spec: SubagentSpec{ID: "prior"}, Done: true, Result: spawn.Result{Passed: false, Branch: "task/prior"}},
	}}
	spec := SubagentSpec{ID: "retry", DependsOn: []string{"dep"}, ContinueFrom: "prior"}
	// continueBase is what the dispatch paths call when ContinueFrom != "" (precedence).
	if got := s.continueBase(st, spec); got != "task/prior" {
		t.Errorf("with continue_from set, base = %q, want the prior attempt task/prior (not the dep)", got)
	}
	// And the dep-based resolver would have returned the dep branch — proving they differ
	// and continue_from is the one the dispatch chooses.
	if dep := s.depTip(st, spec); dep != "task/dep" {
		t.Errorf("sanity: depTip = %q, want task/dep", dep)
	}
}

// strings import kept meaningful: a focused check that the validation message names the
// offending id (so the model can correct it).
func TestContinueFromErrorNamesTheID(t *testing.T) {
	s := &Supervisor{}
	st := &runState{handles: map[string]*Handle{}}
	reason, _ := s.checkSpawnRails(st, SubagentSpec{ID: "new", Role: roster.RoleImplementer, Goal: "g", ContinueFrom: "ghost"})
	if !strings.Contains(reason, "ghost") {
		t.Errorf("validation error should name the missing id; got %q", reason)
	}
}
