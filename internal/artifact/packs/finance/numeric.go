package finance

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// numericMatch compares a model-authored claimed value string against a fact value
// fetched from the source. The rule (documented in floatTolerance): integers compare
// exactly; if either side is a float, they match iff the relative difference is within
// floatTolerance. Both sides are parsed as Go float64 for the comparison, but an
// integer claim against an integer fact must be EXACTLY equal (no tolerance) — so a
// claim of "100" never passes against a fact of "101".
//
// Returns (matched, detail). detail is a bounded, harness-authored note describing the
// comparison outcome; it intentionally states the FETCHED value and the claimed value
// as numbers (not the raw model string) so no unfenced model prose leaks (I7).
func numericMatch(claimed string, fetched float64, fetchedIsInt bool) (bool, string) {
	claimed = strings.TrimSpace(claimed)
	if claimed == "" {
		return false, "claimed value is empty"
	}

	// An integer-vs-integer comparison is exact. We treat a claim as an integer only
	// when it parses as one AND the fetched fact is itself integral.
	if ci, err := strconv.ParseInt(claimed, 10, 64); err == nil && fetchedIsInt {
		if float64(ci) == fetched {
			return true, fmt.Sprintf("exact int match: %d", ci)
		}
		return false, fmt.Sprintf("int mismatch: claimed=%d fetched=%.0f", ci, fetched)
	}

	cf, err := strconv.ParseFloat(claimed, 64)
	if err != nil {
		return false, fmt.Sprintf("claimed value %q is not numeric", truncate(claimed, 64))
	}
	if math.IsNaN(cf) || math.IsInf(cf, 0) {
		return false, "claimed value is not a finite number"
	}

	diff := math.Abs(cf - fetched)
	scale := math.Max(1, math.Abs(fetched))
	if diff <= floatTolerance*scale {
		return true, fmt.Sprintf("float match within %g (fetched=%g)", floatTolerance, fetched)
	}
	return false, fmt.Sprintf("float mismatch beyond %g: claimed=%g fetched=%g", floatTolerance, cf, fetched)
}

// truncate bounds a string for a detail note (defense against a long model field).
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
