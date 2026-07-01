// oracle.go — the Pillar-2 dynamic-routing seam (RTE-T04).
//
// This file DEFINES (never implements) an additive, optional seam the
// orchestrator will consult to make trust-informed routing the default: which
// candidate backends to try and in what order, with what best-of-N sizing.
// The Trust Ledger (internal/trust) IMPLEMENTS this interface WITHOUT importing
// agent — exactly like trust.Selector satisfies Selector. The dependency
// direction stays orchestrator <- trust: the
// orchestrator wires the leaf; the leaf never reaches back.
//
// I2 boundary: a TrustOracle only ORDERS / PRUNES / SIZES candidacy. It never
// decides "done" and never picks a race winner — the verifier judges every race
// (route.Race) and re-runs as the final gate (executeSingle). An oracle is a
// bias on what to attempt and how hard, never an override of the verdict.
//
// DEFAULT-OFF: a nil TrustOracle ⇒ today's static behaviour, byte-identical. The
// PlanRoute / OracleRaceN helpers below let the
// orchestrator call THROUGH a possibly-nil oracle without branching at each call
// site — a nil oracle returns the inputs / default values unchanged.

package agent

import "context"

// RoutePlan is what a TrustOracle returns for one task class: the ordered, pruned
// candidate set plus optional sizing hints. It carries backend NAMES (the same
// currency as Selector / orderBackends), so the orchestrator's existing
// name-indexed paths consume it unchanged.
//
// The sizing hints are OPTIONAL and use a zero-as-unset convention: a hint of 0
// means "no opinion — keep the orchestrator's configured default" (so a plan that
// only reorders candidates leaves race-N exactly as the static
// path would have it). A wired oracle that wants to size sets a value > 0.
type RoutePlan struct {
	// Candidates is the ordered, possibly-pruned set of backend names to try,
	// best-first. An empty slice means "no opinion": the orchestrator keeps its
	// configured candidate set (the helpers below enforce this), so a degenerate
	// oracle can never starve the hot path of a runnable backend.
	Candidates []string

	// RaceN, when > 0, is the oracle's data-driven best-of-N sizing for a
	// verify-fail escalation of this task class (replacing the fixed RaceN flag).
	// 0 ⇒ no opinion: the orchestrator's configured default stands.
	RaceN int
}

// TrustOracle is the optional seam the orchestrator consults to make routing
// trust-informed. It is satisfied by the Trust Ledger out-of-package; the agent
// only declares the shape. Every method is pure read-modelled advice — it orders,
// prunes, or sizes, and never executes, verifies, or decides "done" (I2).
//
//   - Plan returns the ordered/pruned candidate set + sizing hints for a task
//     class given the configured candidate names.
//   - RaceN returns the data-driven best-of-N for a task class, or the supplied
//     default when the oracle has no confident opinion (cold/low-confidence cell).
//
// taskClass is a deterministic keyword bucket (trust.Classify); candidates are
// backend names in configured order.
type TrustOracle interface {
	Plan(ctx context.Context, taskClass string, candidates []string) RoutePlan
	RaceN(taskClass string, def int) int
}

// PlanRoute consults a possibly-nil oracle and returns the candidate ordering the
// orchestrator should use. It is the ONE place the nil/degenerate cases are
// handled, so call sites stay branch-free:
//
//   - nil oracle               ⇒ candidates returned UNCHANGED (static path).
//   - oracle returns no names  ⇒ candidates returned UNCHANGED (never starve the
//     hot path of a runnable backend).
//   - otherwise                ⇒ the oracle's RoutePlan, verbatim.
//
// The returned bool reports whether a non-nil oracle was CONSULTED — not whether
// it changed anything. It is true for any non-nil oracle (even a degenerate one
// that returns the candidates unchanged) and false only for a nil oracle, so the
// caller can log "by": "trust" vs the configured order without re-deriving it. (It
// is deliberately NOT "the candidates differ": a non-nil oracle that returns its
// input verbatim still counts as consulted.)
func PlanRoute(ctx context.Context, o TrustOracle, taskClass string, candidates []string) (RoutePlan, bool) {
	if o == nil {
		return RoutePlan{Candidates: candidates}, false
	}
	plan := o.Plan(ctx, taskClass, candidates)
	if len(plan.Candidates) == 0 {
		// Degenerate plan: keep the configured set but preserve any sizing hints
		// the oracle still expressed (race-N is independent of the candidate list).
		plan.Candidates = candidates
	}
	return plan, true
}

// OracleRaceN returns the best-of-N sizing through a possibly-nil oracle: a nil
// oracle yields the default unchanged (static path), so the orchestrator can size
// a race without branching on whether trust routing is wired.
func OracleRaceN(o TrustOracle, taskClass string, def int) int {
	if o == nil {
		return def
	}
	return o.RaceN(taskClass, def)
}
