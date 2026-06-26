// Package measure is the self-improvement REGRESSION FENCE (Phase 16, Pillar 4,
// docs/ROADMAP-CLOSED-LOOP.md §4 / SIF-T04).
//
// The self-improvement flywheel proposes prompt/skill edits and ships them as
// normal verified, human-gated tasks. Before any such candidate is kept, this
// leaf answers ONE question from MEASURED EVIDENCE, never vibes (principle #9):
// did it actually raise the agent's eval pass-rate? A candidate is kept only if
// AFTER's pass-rate exceeds BEFORE's by at least a configurable margin and does
// not regress on any guarded axis.
//
// This directly guards the C6 feedback-loop pathology: a flywheel that accepts
// changes on a hunch will happily drift downhill. The fence is conservative by
// construction — a tie does NOT pass, a regression NEVER passes, and a missing
// margin defaults to "require a strict improvement". The pass-rate it reads is
// eval.Report.PassRate, which is computed from verifier-judged outcomes (I2):
// the harness scores each case by the verifier, so the delta this fence measures
// can never be a backend self-report.
//
// Design: a PURE stdlib + eval leaf. It imports no orchestrator package — the
// wiring layer (selfimprove / loop) installs this fence, never the reverse — so
// "did it improve" can never reach back into "what is done" (see deps_test.go).
// It is deterministic and side-effect free: same Reports in, same Decision out.
package measure

import "nilcore/eval"

// Delta is the MEASURED change between two eval reports — the before/after of a
// self-improvement candidate. Every field is a pure subtraction of report
// aggregates, so the fence reasons over evidence rather than narrative.
type Delta struct {
	// PassRate is After.PassRate - Before.PassRate. Positive means the candidate
	// raised the verifier-judged pass-rate; negative means it regressed.
	PassRate float64
	// Cost is After.TotalCost - Before.TotalCost. Positive means the candidate
	// got MORE expensive. It is reported for visibility and used only as a
	// tie-break guard (see Fence.Improved); it never on its own admits a
	// candidate that failed the pass-rate bar.
	Cost float64
}

// Compute returns the measured Delta between a baseline (before) and a candidate
// (after) eval report. It is a pure function: no IO, no clock, no randomness.
func Compute(before, after eval.Report) Delta {
	return Delta{
		PassRate: after.PassRate - before.PassRate,
		Cost:     after.TotalCost - before.TotalCost,
	}
}

// epsilon absorbs float64 rounding so a delta meant to land exactly on the
// margin is judged consistently (mirrors internal/budget / internal/blastbudget).
const epsilon = 1e-9

// Fence is the regression fence's configuration. The zero value is a usable,
// maximally-conservative fence: a zero Margin requires a STRICT improvement
// (any positive pass-rate delta beyond float rounding), and CostCeiling <= 0
// means cost is not a gate. Build a stricter fence by setting Margin (and,
// optionally, CostCeiling) explicitly.
type Fence struct {
	// Margin is the minimum pass-rate improvement a candidate must clear to be
	// kept, as an absolute pass-rate delta (e.g. 0.05 = "+5 percentage points").
	// A non-positive Margin defaults conservatively to "require strict
	// improvement": After must beat Before by more than float rounding. Margin
	// is clamped to be at least strictly positive, so a tie can never pass.
	Margin float64
	// CostCeiling, when > 0, additionally rejects a candidate whose cost rose by
	// more than this many dollars even if its pass-rate cleared the margin —
	// so a self-improvement cannot buy pass-rate with runaway spend. A
	// non-positive CostCeiling leaves the cost axis ungated.
	CostCeiling float64
}

// Improved reports whether the AFTER report is a measurably better candidate
// than BEFORE and should therefore be KEPT. It is true only when ALL hold:
//
//   - the pass-rate strictly improved by at least the (clamped-positive) margin
//     — a tie or any regression is rejected (C6 guard);
//   - if a positive CostCeiling is set, the cost did not rise by more than it.
//
// The decision is derived entirely from the two reports' aggregates, so it is a
// MEASURED verdict, not a judgement call. Improved is a pure function.
func (f Fence) Improved(before, after eval.Report) bool {
	return f.Decide(before, after).Keep
}

// Decision is the fence's full, auditable verdict: the measured delta, the
// effective margin applied, the keep/reject outcome, and a short structural
// reason. Reason is a fixed enumerated token (never distilled or model text),
// so it is safe to log and template (I7).
type Decision struct {
	Delta  Delta
	Margin float64 // the effective margin actually applied after clamping
	Keep   bool
	Reason Reason
}

// Reason is a small closed enum naming WHY the fence decided as it did. It is a
// structural field only — never free-form text — so callers may branch on or
// template it without treating any untrusted string as an instruction (I7).
type Reason uint8

const (
	// ReasonRegressed: after's pass-rate is below before's. Never kept.
	ReasonRegressed Reason = iota
	// ReasonTie: after's pass-rate equals before's (within rounding). Never kept.
	ReasonTie
	// ReasonBelowMargin: after improved, but by less than the required margin.
	ReasonBelowMargin
	// ReasonCostCeiling: after cleared the pass-rate margin, but its cost rose by
	// more than the configured CostCeiling.
	ReasonCostCeiling
	// ReasonImproved: after cleared every gate. Kept.
	ReasonImproved
)

// String renders the reason as a stable lowercase token for logs.
func (r Reason) String() string {
	switch r {
	case ReasonRegressed:
		return "regressed"
	case ReasonTie:
		return "tie"
	case ReasonBelowMargin:
		return "below_margin"
	case ReasonCostCeiling:
		return "cost_ceiling"
	case ReasonImproved:
		return "improved"
	default:
		return "unknown"
	}
}

// effectiveMargin clamps a non-positive configured margin up to a strictly
// positive floor (epsilon), so "no margin set" means "require strict
// improvement" and a tie can never satisfy the bar.
func (f Fence) effectiveMargin() float64 {
	if f.Margin > epsilon {
		return f.Margin
	}
	return epsilon
}

// Decide computes the full Decision: the measured delta, the effective margin,
// the keep/reject outcome, and the structural reason. It is the single source
// of truth that Improved is a thin wrapper over. Pure and deterministic.
func (f Fence) Decide(before, after eval.Report) Decision {
	d := Compute(before, after)
	margin := f.effectiveMargin()
	dec := Decision{Delta: d, Margin: margin}

	switch {
	case d.PassRate < -epsilon:
		// Any measured regression is rejected outright — the C6 guard. A change
		// that lowers pass-rate is never kept, regardless of cost or margin.
		dec.Reason = ReasonRegressed
		return dec
	case d.PassRate <= epsilon:
		// A tie (no measurable movement) does not pass: self-improvement must be
		// an improvement, not a wash.
		dec.Reason = ReasonTie
		return dec
	case d.PassRate < margin-epsilon:
		// Positive, but below the required bar.
		dec.Reason = ReasonBelowMargin
		return dec
	}

	// Pass-rate cleared the margin. Apply the optional cost gate last, so a
	// candidate cannot buy pass-rate with unbounded spend.
	if f.CostCeiling > 0 && d.Cost > f.CostCeiling+epsilon {
		dec.Reason = ReasonCostCeiling
		return dec
	}

	dec.Keep = true
	dec.Reason = ReasonImproved
	return dec
}
