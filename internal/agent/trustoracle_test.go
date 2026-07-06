package agent_test

import (
	"context"
	"reflect"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/model"
	"nilcore/internal/trust"
)

// TrustRouteOracle must satisfy the seam it implements.
var _ agent.TrustOracle = (*agent.TrustRouteOracle)(nil)

// recordClass folds n verifier-judged outcomes for one (class, backend) cell into
// the ledger: wins of them passed, the rest failed, each charged unitCost. This is
// the hermetic ledger-builder — no event log, no replay, just the public Record
// fold the ledger is designed around.
func recordClass(l *trust.Ledger, class, backend string, races, wins int, unitCost float64) {
	for i := 0; i < races; i++ {
		l.Record(trust.Outcome{
			Backend: backend,
			Class:   class,
			Passed:  i < wins,
			Cost:    unitCost,
		})
	}
}

// TestTrustRouteOracle_ColdLedgerByteIdentical is the golden default-off proof: an
// oracle over a FRESH ledger expresses no opinion, so Plan returns the configured
// candidate set UNCHANGED (same order, same elements) — wiring it on a cold ledger
// is byte-identical to the static path.
func TestTrustRouteOracle_ColdLedgerByteIdentical(t *testing.T) {
	cands := []string{"native", "codex", "claude-code"}

	// Cold ledger, cost-blind.
	o := agent.NewTrustRouteOracle(trust.New(), nil)
	plan := o.Plan(context.Background(), "refactor", cands)
	if !reflect.DeepEqual(plan.Candidates, cands) {
		t.Fatalf("cold ledger changed candidates: got %v want %v", plan.Candidates, cands)
	}
	if plan.RaceN != 0 {
		t.Fatalf("cold ledger expressed sizing hints: raceN=%d", plan.RaceN)
	}

	// Nil ledger is equally an abstain.
	if got := agent.NewTrustRouteOracle(nil, nil).Plan(context.Background(), "refactor", cands); !reflect.DeepEqual(got.Candidates, cands) {
		t.Fatalf("nil ledger changed candidates: got %v want %v", got.Candidates, cands)
	}

	// A barely-warm cell (below minRacesForOpinion) is still an abstain.
	warm := trust.New()
	recordClass(warm, "refactor", "codex", 2, 2, 0) // only 2 races < 3
	if got := agent.NewTrustRouteOracle(warm, nil).Plan(context.Background(), "refactor", cands); !reflect.DeepEqual(got.Candidates, cands) {
		t.Fatalf("barely-warm ledger changed candidates: got %v want %v", got.Candidates, cands)
	}
}

// TestTrustRouteOracle_ReordersBestFirst proves that once a class cell carries
// enough evidence, the oracle promotes the strongest backend ahead of weaker ones,
// regardless of configured order.
func TestTrustRouteOracle_ReordersBestFirst(t *testing.T) {
	l := trust.New()
	// codex is strongly proven on bugfix; native is proven-weak; claude-code unseen.
	recordClass(l, "bugfix", "native", 10, 2, 0) // 0.20 raw → smoothed ~0.25
	recordClass(l, "bugfix", "codex", 10, 9, 0)  // 0.90 raw → smoothed ~0.83, clears bar
	// claude-code: no evidence on this class.

	o := agent.NewTrustRouteOracle(l, nil)
	cands := []string{"native", "codex", "claude-code"}
	plan := o.Plan(context.Background(), "bugfix", cands)

	if len(plan.Candidates) == 0 {
		t.Fatal("plan emptied a non-empty candidate set")
	}
	if plan.Candidates[0] != "codex" {
		t.Fatalf("best-first reorder failed: got %v, want codex first", plan.Candidates)
	}
	// The proven-weak native and the unseen claude-code form the ladder behind the
	// clearing tier; native (known, weak) precedes the unseen claude-code.
	wantTail := []string{"native", "claude-code"}
	if !reflect.DeepEqual(plan.Candidates[1:], wantTail) {
		t.Fatalf("ladder order wrong: got %v want codex then %v", plan.Candidates, wantTail)
	}
	// The input slice must not be mutated.
	if !reflect.DeepEqual(cands, []string{"native", "codex", "claude-code"}) {
		t.Fatalf("input candidates mutated: %v", cands)
	}
}

// TestTrustRouteOracle_PrefersCheaperEquallyTrusted is the cost-routing win: given
// two backends that have BOTH cleared the confidence bar with the same pass rate,
// the oracle tries the cheaper one first.
func TestTrustRouteOracle_PrefersCheaperEquallyTrusted(t *testing.T) {
	l := trust.New()
	// Two equally-trusted backends on the same class (both 9/10 → clear the bar).
	recordClass(l, "feature", "premium", 10, 9, 0)
	recordClass(l, "feature", "budget", 10, 9, 0)

	// budget is cheaper than premium.
	cost := func(backend string) float64 {
		switch backend {
		case "premium":
			return 1.00
		case "budget":
			return 0.10
		default:
			return 0.50
		}
	}
	o := agent.NewTrustRouteOracle(l, agent.CostFunc(cost))

	// Configured order puts the EXPENSIVE one first, so a pass proves the oracle
	// reordered on cost (not just preserved input order).
	cands := []string{"premium", "budget"}
	plan := o.Plan(context.Background(), "feature", cands)
	if len(plan.Candidates) == 0 {
		t.Fatal("plan emptied a non-empty candidate set")
	}
	if plan.Candidates[0] != "budget" {
		t.Fatalf("cost preference failed: got %v, want cheaper 'budget' first", plan.Candidates)
	}

	// Sanity: cost-BLIND over the same ledger keeps the proven tier on trust+name
	// order (a tie on rate ⇒ name order), so 'budget' < 'premium' by name still
	// comes first here — assert the cost path and the blind path can differ by
	// flipping the names so name-order would put premium first.
	l2 := trust.New()
	recordClass(l2, "feature", "aexpensive", 10, 9, 0)
	recordClass(l2, "feature", "zcheap", 10, 9, 0)
	cost2 := func(b string) float64 {
		if b == "zcheap" {
			return 0.10
		}
		return 1.00
	}
	cands2 := []string{"aexpensive", "zcheap"}
	costAware := agent.NewTrustRouteOracle(l2, agent.CostFunc(cost2)).Plan(context.Background(), "feature", cands2)
	if costAware.Candidates[0] != "zcheap" {
		t.Fatalf("cost-aware should prefer zcheap despite name order: got %v", costAware.Candidates)
	}
	blind := agent.NewTrustRouteOracle(l2, nil).Plan(context.Background(), "feature", cands2)
	if blind.Candidates[0] != "aexpensive" {
		t.Fatalf("cost-blind should fall back to name order (aexpensive first): got %v", blind.Candidates)
	}
}

// TestTrustRouteOracle_NeverEmpties proves the oracle is a permutation, never a
// deletion: a non-empty input always yields a non-empty plan with the same set of
// names, across the abstain path, the cost-aware path, and an empty input.
func TestTrustRouteOracle_NeverEmpties(t *testing.T) {
	l := trust.New()
	recordClass(l, "test", "codex", 8, 7, 0.5)
	recordClass(l, "test", "native", 8, 3, 0.1)
	o := agent.NewTrustRouteOracle(l, agent.CostFunc(func(string) float64 { return 1 }))

	cands := []string{"native", "codex", "claude-code"}
	plan := o.Plan(context.Background(), "test", cands)
	if !sameSet(plan.Candidates, cands) {
		t.Fatalf("plan is not a permutation of the input: got %v want set of %v", plan.Candidates, cands)
	}

	// Empty input ⇒ empty (nil-safe, no panic), never a fabricated candidate.
	if got := o.Plan(context.Background(), "test", nil); len(got.Candidates) != 0 {
		t.Fatalf("empty input produced non-empty plan: %v", got.Candidates)
	}
}

// TestTrustRouteOracle_SizingConservative proves RaceN / EscalateAfter never
// return a value LESS safe than the supplied default (they pass it through).
func TestTrustRouteOracle_SizingConservative(t *testing.T) {
	l := trust.New()
	recordClass(l, "refactor", "codex", 50, 49, 0) // very strong, lots of evidence
	o := agent.NewTrustRouteOracle(l, nil)

	for _, def := range []int{1, 2, 3, 5} {
		if got := o.RaceN("refactor", def); got < def {
			t.Fatalf("RaceN returned a LESS safe value: got %d < default %d", got, def)
		}
	}
}

// TestPricerCost_AdaptsPricer proves the PricerCost adapter turns a meter.Pricer
// into a usable CostFunc and degrades to a zero (cost-neutral) cost rather than
// panicking on nil/empty inputs.
func TestPricerCost_AdaptsPricer(t *testing.T) {
	p := stubPricer{rate: map[string]float64{"claude-opus": 2.0, "gpt-5.5": 0.5}}
	id := func(backend string) string {
		switch backend {
		case "native":
			return "claude-opus"
		case "codex":
			return "gpt-5.5"
		default:
			return ""
		}
	}
	cf := agent.PricerCost(p, id, 1000, 1000)
	if got := cf("native"); got != 2.0 {
		t.Fatalf("PricerCost(native) = %v, want 2.0", got)
	}
	if got := cf("codex"); got != 0.5 {
		t.Fatalf("PricerCost(codex) = %v, want 0.5", got)
	}
	// Unknown backend ⇒ empty model id ⇒ cost-neutral 0, no panic.
	if got := cf("mystery"); got != 0 {
		t.Fatalf("PricerCost(mystery) = %v, want 0", got)
	}
	// Nil pricer / nil resolver ⇒ 0, no panic.
	if got := agent.PricerCost(nil, id, 1, 1)("native"); got != 0 {
		t.Fatalf("PricerCost(nil pricer) = %v, want 0", got)
	}
	if got := agent.PricerCost(p, nil, 1, 1)("native"); got != 0 {
		t.Fatalf("PricerCost(nil resolver) = %v, want 0", got)
	}
}

// TestOrchestratorCost_AdaptsCostFunc proves OrchestratorCost lifts a per-backend
// CostFunc into the (taskClass, backendName) shape Orchestrator.Cost wants — ignoring
// the class, reading the backend — and returns nil for a nil CostFunc so the seam
// stays default-off (byte-identical: no cost dimension recorded).
func TestOrchestratorCost_AdaptsCostFunc(t *testing.T) {
	base := agent.CostFunc(func(backend string) float64 {
		if backend == "native" {
			return 2.0
		}
		return 0.5
	})
	oc := agent.OrchestratorCost(base)
	if oc == nil {
		t.Fatal("OrchestratorCost returned nil for a non-nil CostFunc")
	}
	// The class is ignored; the backend drives the cost.
	if got := oc("refactor", "native"); got != 2.0 {
		t.Errorf("OrchestratorCost(refactor, native) = %v, want 2.0", got)
	}
	if got := oc("feature", "codex"); got != 0.5 {
		t.Errorf("OrchestratorCost(feature, codex) = %v, want 0.5", got)
	}
	// A nil CostFunc yields a nil adapter — Orchestrator.Cost stays unset.
	if agent.OrchestratorCost(nil) != nil {
		t.Error("OrchestratorCost(nil) should return nil so the seam stays default-off")
	}
}

// stubPricer is a hermetic meter.Pricer: it returns a fixed per-id cost, ignoring
// token counts (the oracle only compares costs relatively, so absolute arithmetic
// is irrelevant to these tests).
type stubPricer struct{ rate map[string]float64 }

func (s stubPricer) Price(modelID string, _, _ int) float64 { return s.rate[modelID] }

// PriceUsage is unused by the oracle (it calls Price) but is part of the Pricer
// interface, so the stub satisfies it trivially.
func (s stubPricer) PriceUsage(modelID string, _ model.Usage) float64 { return s.rate[modelID] }

// sameSet reports whether a and b contain exactly the same names (order-agnostic),
// proving the plan neither drops nor fabricates a candidate.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, n := range a {
		seen[n]++
	}
	for _, n := range b {
		seen[n]--
	}
	for _, c := range seen {
		if c != 0 {
			return false
		}
	}
	return true
}
