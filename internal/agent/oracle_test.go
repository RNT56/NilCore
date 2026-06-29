package agent_test

import (
	"context"
	"reflect"
	"testing"

	"nilcore/internal/agent"
)

// fakeOracle is a hermetic TrustOracle that reverses the candidate order and
// returns fixed sizing hints — enough to prove the seam carries an oracle's
// decision through verbatim, without any trust/store dependency.
type fakeOracle struct {
	raceN int
	prune bool // when true, Plan drops all but the first candidate
}

func (f fakeOracle) Plan(_ context.Context, _ string, candidates []string) agent.RoutePlan {
	out := make([]string, len(candidates))
	for i, n := range candidates {
		out[len(candidates)-1-i] = n // reverse
	}
	if f.prune && len(out) > 1 {
		out = out[:1]
	}
	return agent.RoutePlan{Candidates: out, RaceN: f.raceN}
}

func (f fakeOracle) RaceN(_ string, def int) int {
	if f.raceN > 0 {
		return f.raceN
	}
	return def
}

// emptyPlanOracle is the degenerate case: a Plan that returns no candidates. The
// helper must keep the static set rather than starve the hot path.
type emptyPlanOracle struct{}

func (emptyPlanOracle) Plan(context.Context, string, []string) agent.RoutePlan {
	return agent.RoutePlan{}
}
func (emptyPlanOracle) RaceN(_ string, def int) int { return def }

// TestPlanRoute_NilOracleByteIdentical is the golden default-off proof: with a
// nil oracle the candidate set is returned UNCHANGED and "applied" is false, so
// the orchestrator's static ordering is byte-identical to before this seam.
func TestPlanRoute_NilOracleByteIdentical(t *testing.T) {
	cands := []string{"native", "codex", "claude-code"}
	plan, applied := agent.PlanRoute(context.Background(), nil, "refactor", cands)
	if applied {
		t.Fatalf("nil oracle must not be applied, got applied=true")
	}
	if !reflect.DeepEqual(plan.Candidates, cands) {
		t.Fatalf("nil oracle changed candidates: got %v want %v", plan.Candidates, cands)
	}
	if plan.RaceN != 0 {
		t.Fatalf("nil oracle expressed sizing hints: raceN=%d", plan.RaceN)
	}
}

// TestOracleSizing_NilDefaultsUnchanged proves the sizing helpers pass the
// orchestrator's configured defaults straight through when no oracle is wired.
func TestOracleSizing_NilDefaultsUnchanged(t *testing.T) {
	if got := agent.OracleRaceN(nil, "refactor", 3); got != 3 {
		t.Fatalf("OracleRaceN(nil) = %d, want default 3", got)
	}
}

// TestPlanRoute_FakeReordersAndSizes proves a wired oracle's ordering and sizing
// flow through the helper verbatim.
func TestPlanRoute_FakeReordersAndSizes(t *testing.T) {
	cands := []string{"native", "codex", "claude-code"}
	o := fakeOracle{raceN: 4}
	plan, applied := agent.PlanRoute(context.Background(), o, "bugfix", cands)
	if !applied {
		t.Fatalf("wired oracle must be applied, got applied=false")
	}
	want := []string{"claude-code", "codex", "native"}
	if !reflect.DeepEqual(plan.Candidates, want) {
		t.Fatalf("reorder mismatch: got %v want %v", plan.Candidates, want)
	}
	if plan.RaceN != 4 {
		t.Fatalf("sizing not carried: raceN=%d", plan.RaceN)
	}
	// The input slice must not be mutated by the oracle path.
	if !reflect.DeepEqual(cands, []string{"native", "codex", "claude-code"}) {
		t.Fatalf("input candidates mutated: %v", cands)
	}
}

// TestPlanRoute_FakePrunes proves the oracle may legitimately drop candidates.
func TestPlanRoute_FakePrunes(t *testing.T) {
	cands := []string{"native", "codex", "claude-code"}
	plan, applied := agent.PlanRoute(context.Background(), fakeOracle{prune: true}, "doc", cands)
	if !applied {
		t.Fatalf("wired oracle must be applied")
	}
	// reverse-then-prune-to-1 ⇒ the last input is the sole survivor.
	if len(plan.Candidates) != 1 || plan.Candidates[0] != "claude-code" {
		t.Fatalf("prune mismatch: got %v want [claude-code]", plan.Candidates)
	}
}

// TestPlanRoute_EmptyPlanKeepsStatic proves a degenerate oracle that returns no
// names never starves the hot path: the configured candidates are preserved.
func TestPlanRoute_EmptyPlanKeepsStatic(t *testing.T) {
	cands := []string{"native", "codex"}
	plan, applied := agent.PlanRoute(context.Background(), emptyPlanOracle{}, "refactor", cands)
	if !applied {
		t.Fatalf("a wired (even degenerate) oracle reports applied=true")
	}
	if !reflect.DeepEqual(plan.Candidates, cands) {
		t.Fatalf("empty plan did not fall back to static set: got %v want %v", plan.Candidates, cands)
	}
}

// TestOracleSizing_FakeOverrides proves the sizing helpers return the oracle's
// value when it has an opinion and the default otherwise.
func TestOracleSizing_FakeOverrides(t *testing.T) {
	o := fakeOracle{raceN: 5}
	if got := agent.OracleRaceN(o, "bugfix", 2); got != 5 {
		t.Fatalf("OracleRaceN = %d, want oracle value 5", got)
	}
}
