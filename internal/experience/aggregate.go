package experience

import (
	"sort"
	"time"
)

// Aggregate is the per-task-class outcome rollup: how often a class of work
// passed the verifier, and the typical cost/latency/recency of those contests.
// It is derived only from verifier verdicts (race_outcome events), so a high
// pass-rate here means "the verifier said yes", never "a backend claimed done".
type Aggregate struct {
	Class         string
	Races         int       // verifier-judged contests observed
	Passes        int       // contests the verifier passed
	MedianCostUSD float64   // median per-contest dollar cost (0 if unrecorded)
	MedianLatency float64   // median per-contest latency in nanoseconds (0 if unrecorded)
	LastSeen      time.Time // most recent contest time (zero if none)
}

// median returns the median of xs (0 for an empty slice). It copies before
// sorting so the caller's sample slice is left untouched.
func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// floatOf reads a JSON-decoded number (which encoding/json represents as
// float64) from an untyped Detail value. A missing or non-numeric value reads as
// absent, never as zero-that-counts.
func floatOf(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}
