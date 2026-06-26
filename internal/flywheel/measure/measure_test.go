package measure

import (
	"math"
	"testing"

	"nilcore/eval"
)

// closeTo reports whether got is within float rounding of want, so exact-value
// assertions are not defeated by float64 subtraction (e.g. 0.80-0.50 yields
// 0.30000000000000004).
func closeTo(got, want float64) bool { return math.Abs(got-want) <= epsilon }

// rep is a tiny constructor for an eval.Report with only the aggregate fields
// the fence reads, keeping the table compact and the intent obvious.
func rep(passRate, cost float64) eval.Report {
	return eval.Report{PassRate: passRate, TotalCost: cost}
}

func TestComputeDelta(t *testing.T) {
	got := Compute(rep(0.50, 10), rep(0.80, 12))
	if !closeTo(got.PassRate, 0.30) {
		t.Errorf("PassRate delta = %v, want 0.30", got.PassRate)
	}
	if !closeTo(got.Cost, 2) {
		t.Errorf("Cost delta = %v, want 2", got.Cost)
	}
}

func TestImproved(t *testing.T) {
	tests := []struct {
		name   string
		fence  Fence
		before eval.Report
		after  eval.Report
		want   bool
		reason Reason
	}{
		{
			name:   "strict improvement passes default fence",
			before: rep(0.50, 1),
			after:  rep(0.60, 1),
			want:   true,
			reason: ReasonImproved,
		},
		{
			name:   "regression fails (C6 guard)",
			before: rep(0.60, 1),
			after:  rep(0.50, 1),
			want:   false,
			reason: ReasonRegressed,
		},
		{
			name:   "large regression fails even when cheaper",
			before: rep(0.90, 100),
			after:  rep(0.10, 1),
			want:   false,
			reason: ReasonRegressed,
		},
		{
			name:   "tie fails (no improvement is not improvement)",
			before: rep(0.70, 1),
			after:  rep(0.70, 1),
			want:   false,
			reason: ReasonTie,
		},
		{
			name:   "tie fails even when candidate is cheaper",
			before: rep(0.70, 100),
			after:  rep(0.70, 1),
			want:   false,
			reason: ReasonTie,
		},
		{
			name:   "improvement below explicit margin fails",
			fence:  Fence{Margin: 0.10},
			before: rep(0.50, 1),
			after:  rep(0.55, 1), // +0.05 < 0.10
			want:   false,
			reason: ReasonBelowMargin,
		},
		{
			name:   "improvement exactly at margin passes",
			fence:  Fence{Margin: 0.10},
			before: rep(0.50, 1),
			after:  rep(0.60, 1), // +0.10 == margin
			want:   true,
			reason: ReasonImproved,
		},
		{
			name:   "improvement above margin passes",
			fence:  Fence{Margin: 0.10},
			before: rep(0.50, 1),
			after:  rep(0.75, 1), // +0.25 > 0.10
			want:   true,
			reason: ReasonImproved,
		},
		{
			name:   "negative configured margin behaves as strict improvement",
			fence:  Fence{Margin: -0.5},
			before: rep(0.50, 1),
			after:  rep(0.50, 1), // tie still fails
			want:   false,
			reason: ReasonTie,
		},
		{
			name:   "cost ceiling rejects a too-expensive improvement",
			fence:  Fence{Margin: 0.05, CostCeiling: 1.0},
			before: rep(0.50, 10),
			after:  rep(0.80, 12), // +0.30 pass-rate but +2 cost > 1.0 ceiling
			want:   false,
			reason: ReasonCostCeiling,
		},
		{
			name:   "cost within ceiling keeps an improvement",
			fence:  Fence{Margin: 0.05, CostCeiling: 5.0},
			before: rep(0.50, 10),
			after:  rep(0.80, 12), // +2 cost < 5.0 ceiling
			want:   true,
			reason: ReasonImproved,
		},
		{
			name:   "cheaper improvement passes cost gate",
			fence:  Fence{Margin: 0.05, CostCeiling: 1.0},
			before: rep(0.50, 10),
			after:  rep(0.80, 9), // cost dropped
			want:   true,
			reason: ReasonImproved,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fence.Improved(tt.before, tt.after); got != tt.want {
				t.Errorf("Improved() = %v, want %v", got, tt.want)
			}
			dec := tt.fence.Decide(tt.before, tt.after)
			if dec.Keep != tt.want {
				t.Errorf("Decide().Keep = %v, want %v", dec.Keep, tt.want)
			}
			if dec.Reason != tt.reason {
				t.Errorf("Decide().Reason = %v, want %v", dec.Reason, tt.reason)
			}
		})
	}
}

// TestMarginPrecision pins the float-rounding boundary: a delta a hair under the
// margin must fail and a delta a hair over must pass, so the fence is not fooled
// by accumulated float error in either direction.
func TestMarginPrecision(t *testing.T) {
	f := Fence{Margin: 0.10}
	// 0.30 - 0.20 == 0.10 in exact arithmetic but accumulates rounding; it must
	// still be accepted as meeting the margin.
	if !f.Improved(rep(0.20, 0), rep(0.30, 0)) {
		t.Errorf("delta at margin should pass despite float rounding")
	}
	// A clear sub-margin improvement must fail.
	if f.Improved(rep(0.20, 0), rep(0.25, 0)) {
		t.Errorf("delta below margin should fail")
	}
}

// TestZeroValueFenceIsStrict documents that the zero Fence is usable and
// conservative: it requires a strict improvement and applies no cost gate.
func TestZeroValueFenceIsStrict(t *testing.T) {
	var f Fence
	if f.effectiveMargin() != epsilon {
		t.Errorf("zero fence effectiveMargin = %v, want epsilon %v", f.effectiveMargin(), epsilon)
	}
	if f.Improved(rep(0.50, 1), rep(0.50, 1)) {
		t.Errorf("zero fence must reject a tie")
	}
	if !f.Improved(rep(0.50, 999), rep(0.51, 0.0001)) {
		t.Errorf("zero fence must keep a strict improvement regardless of cost")
	}
}

func TestReasonString(t *testing.T) {
	cases := map[Reason]string{
		ReasonRegressed:   "regressed",
		ReasonTie:         "tie",
		ReasonBelowMargin: "below_margin",
		ReasonCostCeiling: "cost_ceiling",
		ReasonImproved:    "improved",
		Reason(200):       "unknown",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("Reason(%d).String() = %q, want %q", r, got, want)
		}
	}
}

// TestDeterministic asserts the fence is a pure function: identical inputs yield
// an identical Decision every call (no clock, no randomness, no state).
func TestDeterministic(t *testing.T) {
	f := Fence{Margin: 0.07, CostCeiling: 3}
	before, after := rep(0.41, 5), rep(0.55, 6)
	first := f.Decide(before, after)
	for i := 0; i < 100; i++ {
		if got := f.Decide(before, after); got != first {
			t.Fatalf("Decide not deterministic: call %d = %+v, first = %+v", i, got, first)
		}
	}
}
