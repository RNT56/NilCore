package benchmark

// numeric.go holds the pure, side-effect-free statistics this pack reduces a
// benchmark to. It reaches NO box and parses NO model field — every function here is
// a deterministic function of a []float64 of samples. WHAT IS BEING VERIFIED, stated
// plainly so no caller mistakes it: this pack verifies a CLAIMED BOUND (an aggregate
// op a threshold) AND that the run-to-run VARIANCE stays inside a ceiling, computed
// over the VERIFIER'S OWN re-runs (script_threshold) or over a provided sample array
// as a secondary self-consistency check (variance_bounded). It NEVER claims to
// reproduce an exact wall-clock number — wall-clock is host- and load-dependent, so a
// point-equality assertion would be noise. The bound + the coefficient of variation
// are the two things that are honestly checkable across machines, and they are all
// this pack asserts.
//
// Coefficient of variation (CV) is the unitless dispersion measure we use because the
// metric's magnitude is unknown a priori (ns/op vs MB/s vs a custom score): CV =
// stddev/|mean|, so a "within 5%" variance bound is expressible independent of units.
// We use the POPULATION stddev (divide by N, not N-1): the samples ARE the whole set
// of runs the verifier performed, not a sample drawn from a larger population, so the
// population form is the correct, and the more conservative-for-small-N, estimator.

import "math"

// mean returns the arithmetic mean of xs. Caller guarantees len(xs) >= 1 (every call
// site checks minSamples first); an empty slice returns 0 rather than NaN so a missed
// guard fails closed-ish rather than poisoning a comparison.
func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// stddev returns the POPULATION standard deviation of xs (divide by N). It is the
// dispersion of the verifier's own re-runs around their mean. len(xs) < 1 returns 0.
// A single sample has zero dispersion by definition (the variance bound is then
// trivially satisfied) — which is exactly why the checks separately require >= 2
// samples before they will assert a variance verdict at all.
func stddev(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	m := mean(xs)
	var ss float64
	for _, x := range xs {
		d := x - m
		ss += d * d
	}
	return math.Sqrt(ss / float64(n))
}

// cv returns the coefficient of variation: stddev/|mean|. It is the unit-free measure
// the variance ceiling is expressed against. Guards:
//   - a non-finite sample (NaN/Inf) makes CV NaN, which compareCV treats as a FAILED
//     bound (an unmeasurable spread is never "within ceiling") — fail-closed.
//   - a mean of exactly 0 with any non-zero spread is infinite relative dispersion;
//     we return +Inf so the ceiling comparison fails. A mean of 0 with zero spread
//     (all samples 0) returns 0 (perfectly consistent, if degenerate).
func cv(xs []float64) float64 {
	for _, x := range xs {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return math.NaN()
		}
	}
	m := mean(xs)
	sd := stddev(xs)
	if m == 0 {
		if sd == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return sd / math.Abs(m)
}

// withinCV reports whether the coefficient of variation of xs is at or below ceiling.
// A NaN CV (non-finite sample) or a NaN/negative ceiling is never "within" — the
// assertion fails closed. Equality (cv == ceiling) passes: the ceiling is inclusive.
func withinCV(xs []float64, ceiling float64) bool {
	if math.IsNaN(ceiling) || ceiling < 0 {
		return false
	}
	c := cv(xs)
	if math.IsNaN(c) || math.IsInf(c, 0) {
		return false
	}
	return c <= ceiling
}

// compareBound asserts (aggregate op bound) where op is "<=" or ">=". The aggregate is
// the mean of the verifier's samples — the central tendency of its own re-runs, not
// any single lucky/unlucky run and not a worker-supplied number. A non-finite
// aggregate or bound, or an unrecognized op, returns false (fail-closed): an
// un-evaluable comparison is never a pass.
func compareBound(aggregate float64, op string, bound float64) bool {
	if math.IsNaN(aggregate) || math.IsInf(aggregate, 0) {
		return false
	}
	if math.IsNaN(bound) || math.IsInf(bound, 0) {
		return false
	}
	switch op {
	case opLE:
		return aggregate <= bound
	case opGE:
		return aggregate >= bound
	default:
		return false
	}
}
