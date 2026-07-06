// trustoracle.go — the first concrete TrustOracle (RTE-T06): cost-aware,
// trust-informed routing backed by the Trust Ledger.
//
// oracle.go DEFINES the seam; this file IMPLEMENTS it. TrustRouteOracle reads the
// per-(class, backend) cells the ledger already folds from verifier-judged
// outcomes (trust.Ledger.ClassStandings) and turns "how has this backend done on
// THIS class of task, and what did it cost me" into an ordered/pruned candidate
// set. It is a leaf consumer: it imports trust + meter (both of which never reach
// back into agent — go list -deps confirms no cycle), so the dependency direction
// stays orchestrator <- {trust, meter}.
//
// I2 boundary, restated at the implementation: this oracle ONLY orders, prunes,
// and (conservatively) sizes candidacy. It never executes a backend, never judges
// a race, never decides "done" — the verifier still judges every race (route.Race)
// and re-runs as the final gate (executeSingle). Mis-ranking only changes which
// backend is *tried first*; it can never ship unverified work.
//
// DEFAULT-OFF / cold-start safety: a TrustRouteOracle over a FRESH ledger (no
// evidence for the class) returns the candidate set UNCHANGED, so wiring it on a
// cold ledger is byte-identical to the static path. Only once a class cell has
// crossed a confidence bar does the oracle express an opinion.

package agent

import (
	"context"
	"sort"

	"nilcore/internal/meter"
	"nilcore/internal/trust"
)

// CostFunc returns the expected dollar cost of attempting one task with the named
// backend. It is the injectable cost seam: callers can wrap a meter.Pricer (see
// PricerCost) or supply any other estimate. A nil CostFunc means "cost-blind" —
// the oracle then orders purely by per-class trust, exactly as a non-cost-aware
// ledger router would. The returned cost is advisory only; like every oracle
// output it biases attempt order and never gates shipping (I2).
type CostFunc func(backend string) float64

// confidenceBar is the minimum smoothed per-class pass rate a backend must reach
// before the oracle treats its cell as "proven enough to prefer on cost". It sits
// above the 0.5 unproven prior so a backend that is merely break-even (or worse)
// is never promoted ahead of a stronger one just because it is cheaper. Backends
// at or above the bar form the "clearing" tier the cost preference applies within;
// backends below it keep their trust order and sit behind the clearing tier (they
// are the post-fail escalation ladder, costliest-trust-last).
const confidenceBar = 0.6

// minRacesForOpinion is the cold-start guard: a class cell with fewer than this
// many verifier-judged races carries too little signal to reorder on. Until EVERY
// candidate with any evidence is at least this well-sampled the oracle abstains
// (returns the candidates unchanged), so a fresh — or barely-warm — ledger is
// byte-identical to the static path. The threshold matches the Laplace prior's
// "evidence must overcome the 0.5 prior" intent: a single lucky sample can never
// flip the order.
const minRacesForOpinion = 3

// TrustRouteOracle is a cost-aware agent.TrustOracle backed by a *trust.Ledger.
// It orders/prunes candidates by the per-class smoothed pass rate the ledger has
// earned, breaking ties (and, within the proven tier, preferring) toward the
// cheaper backend via an optional CostFunc. RaceN / EscalateAfter stay
// conservative: they only ever return a value at least as safe as the supplied
// default (see those methods).
//
// The zero value is not usable; construct via NewTrustRouteOracle. A nil
// *TrustRouteOracle is a valid no-op oracle (all methods degrade to "no opinion"),
// so a caller can hold a possibly-nil *TrustRouteOracle and still route through
// the nil-safe oracle.go helpers.
type TrustRouteOracle struct {
	ledger *trust.Ledger
	cost   CostFunc // nil ⇒ cost-blind ordering
}

// NewTrustRouteOracle builds a cost-aware oracle over the given ledger. cost may
// be nil (cost-blind: order purely by per-class trust). A nil ledger yields an
// oracle that always abstains (no class cells ⇒ no opinion ⇒ candidates
// unchanged), which is the safe cold-start default.
func NewTrustRouteOracle(ledger *trust.Ledger, cost CostFunc) *TrustRouteOracle {
	return &TrustRouteOracle{ledger: ledger, cost: cost}
}

// PricerCost adapts a meter.Pricer into a CostFunc by pricing a fixed nominal
// token budget per candidate, given a backend→model-id resolver. It is a
// convenience for the common wiring (the orchestrator knows which model id each
// backend runs); a caller with a better per-backend estimate can supply its own
// CostFunc instead. The nominal in/out token counts only need to be CONSISTENT
// across candidates — the oracle compares costs relative to one another, it never
// charges them — so the absolute figure is unimportant. A nil pricer or a
// modelID resolver that returns "" for a backend yields a zero cost for that
// backend (cost-neutral), never a panic.
func PricerCost(p meter.Pricer, modelID func(backend string) string, nominalIn, nominalOut int) CostFunc {
	return func(backend string) float64 {
		if p == nil || modelID == nil {
			return 0
		}
		id := modelID(backend)
		if id == "" {
			return 0
		}
		return p.Price(id, nominalIn, nominalOut)
	}
}

// OrchestratorCost adapts a per-backend CostFunc into the shape Orchestrator.Cost
// wants — func(taskClass, backendName string) float64 — so ONE cost source feeds
// BOTH cost-aware seams: the oracle's routing ORDER (via CostFunc, above) and the
// per-race cost METADATA the orchestrator records for trust.Replay to fold into the
// learned per-(class, backend) cost cell (RTE-T06). The task class is ignored: the
// nominal-token pricing PricerCost uses is class-independent (it prices a backend's
// model, not the task), and the orchestrator already threads the class alongside the
// recorded cost — so the cell is keyed by (class, backend) even though the cost value
// itself only varies by backend. A nil CostFunc yields nil (Orchestrator.Cost stays
// unset ⇒ no cost dimension recorded, byte-identical). Cost is metadata only (I7) and
// never gates (I2).
func OrchestratorCost(cost CostFunc) func(taskClass, backendName string) float64 {
	if cost == nil {
		return nil
	}
	return func(_, backendName string) float64 {
		return cost(backendName)
	}
}

// Plan orders and (never) prunes the candidate set for one task class using the
// ledger's per-class evidence, made cost-aware via the optional CostFunc.
//
// The algorithm, designed to be cold-safe and I2-respecting:
//
//  1. ABSTAIN WHEN COLD. If the oracle has no usable opinion for this class —
//     nil ledger, no recorded cell for any candidate, or insufficiently-sampled
//     cells (see minRacesForOpinion) — return the candidates UNCHANGED. This is
//     the byte-identical-to-static path a fresh ledger always takes.
//
//  2. SPLIT INTO A CLEARING TIER AND A LADDER. Candidates whose smoothed per-class
//     pass rate is at or above confidenceBar form the "clearing" tier; the rest
//     (including candidates the ledger has never seen) form the post-fail
//     escalation ladder, kept behind the clearing tier.
//
//  3. ORDER THE CLEARING TIER BY COST, THEN TRUST. Among backends that have all
//     cleared the bar, prefer the CHEAPEST (the cost-routing win: don't pay for a
//     premium backend when a cheaper one is proven good enough), breaking cost
//     ties toward the higher pass rate, then name. A nil/cost-blind CostFunc makes
//     every cost equal, so this degenerates to pure trust order.
//
//  4. ORDER THE LADDER BY TRUST (costliest-trust escalation). Below the bar, order
//     by smoothed pass rate (best-first) so escalation climbs toward the
//     strongest-but-pricier option; unseen candidates sit last in their original
//     relative order (no earned evidence ⇒ a fallback, never a default).
//
// The candidate set is never emptied: it is a permutation (plus tier split) of the
// input, so a non-empty input always yields a non-empty plan. The input slice is
// not mutated.
func (o *TrustRouteOracle) Plan(_ context.Context, taskClass string, candidates []string) RoutePlan {
	if o == nil || o.ledger == nil || len(candidates) == 0 {
		return RoutePlan{Candidates: candidates}
	}

	// Project the class cells we have onto the configured candidate set.
	cells := map[string]trust.ClassStat{}
	for _, c := range o.ledger.ClassStandings(taskClass) {
		cells[c.Backend] = c
	}

	// Cold-start guard: abstain unless at least one candidate has a
	// sufficiently-sampled cell. Without crossing this bar the order is unchanged,
	// so a fresh (or barely-warm) ledger is byte-identical to static.
	if !hasConfidentCell(candidates, cells) {
		return RoutePlan{Candidates: candidates}
	}

	type item struct {
		name   string
		idx    int     // original position, for a stable tiebreak among unseen
		known  bool    // ledger has a cell for this candidate
		clears bool    // known AND smoothed rate >= confidenceBar
		rate   float64 // smoothed per-class pass rate (0.5 prior when unknown)
		cost   float64 // CostFunc estimate (0 when cost-blind)
	}
	items := make([]item, len(candidates))
	for i, name := range candidates {
		it := item{name: name, idx: i, rate: 0.5}
		if c, ok := cells[name]; ok {
			it.known = true
			it.rate = smoothedRate(c)
			it.clears = it.rate >= confidenceBar
		}
		if o.cost != nil {
			it.cost = o.cost(name)
		}
		items[i] = it
	}

	sort.SliceStable(items, func(a, b int) bool {
		x, y := items[a], items[b]
		// Clearing tier (proven good enough) always precedes the ladder.
		if x.clears != y.clears {
			return x.clears
		}
		if x.clears {
			// Within the proven tier: cheapest first (the cost-routing win), then
			// stronger trust, then name for determinism.
			if !floatEq(x.cost, y.cost) {
				return x.cost < y.cost
			}
			if !floatEq(x.rate, y.rate) {
				return x.rate > y.rate
			}
			return x.name < y.name
		}
		// Ladder (below the bar / unseen): known ahead of unseen; among known,
		// stronger trust first (escalation climbs toward the strongest option);
		// unseen keep their original relative order.
		if x.known != y.known {
			return x.known
		}
		if x.known {
			if !floatEq(x.rate, y.rate) {
				return x.rate > y.rate
			}
			return x.name < y.name
		}
		return x.idx < y.idx
	})

	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.name
	}
	return RoutePlan{Candidates: out}
}

// RaceN returns the data-driven best-of-N for a class, never LESS safe than the
// supplied default. Trust evidence can only justify SMALLER races (a class with a
// strongly-proven cheapest backend needs less hedging), but shrinking a race is a
// safety reduction the verifier-gated design forbids us to take unilaterally here:
// fewer parallel attempts means fewer chances to clear the verifier. So this stays
// at the default for now — a deliberately conservative no-op that can be widened
// to GROW races (the only safe direction) in a later task.
func (o *TrustRouteOracle) RaceN(_ string, def int) int {
	return def
}

// hasConfidentCell reports whether at least one candidate has a class cell sampled
// enough to act on (>= minRacesForOpinion races). This is the cold-start gate: no
// confident cell ⇒ the oracle abstains and returns candidates unchanged.
func hasConfidentCell(candidates []string, cells map[string]trust.ClassStat) bool {
	for _, name := range candidates {
		if c, ok := cells[name]; ok && c.Races >= minRacesForOpinion {
			return true
		}
	}
	return false
}

// smoothedRate is the Laplace ("rule of succession") smoothed pass rate for a
// class cell — the SAME smoothing the trust ledger ranks by (alpha = 1 toward a
// 0.5 prior), recomputed here so the oracle's confidence-bar and cost-tier logic
// agree with the ledger's own ordering without reaching into trust's unexported
// scorer. A zero-race cell scores exactly 0.5 (unproven, not trusted).
func smoothedRate(c trust.ClassStat) float64 {
	const alpha = 1.0
	return (float64(c.Wins) + alpha) / (float64(c.Races) + 2*alpha)
}

// floatEq compares two costs/rates with a small epsilon so floating-point noise
// (e.g. equal prices computed via different token arithmetic) is treated as a tie
// and resolved by the deterministic secondary keys, not by an unstable < on
// near-equal floats.
func floatEq(a, b float64) bool {
	const eps = 1e-12
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}
