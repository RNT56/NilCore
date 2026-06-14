// Package impact answers "what does this change touch, and where is the bug?"
// (P3-T15). Two complementary views over the code graph:
//
//   - ImpactSet/AffectedTests — forward-of-blame: the transitive *callers* of a
//     changed symbol (reverse reachability). Editing a leaf ripples up to every
//     caller, so that caller set is exactly what must be re-checked; the subset
//     whose names start with "Test" is the test suite to re-run for the change.
//   - Localize — spectrum-based fault localization (SBFL). Given test coverage
//     (which symbols each failing/passing test executed), rank symbols by how
//     selectively the failures touch them, using the Ochiai suspiciousness
//     metric. This points a debugging agent at the likely culprit first.
//
// Reverse reachability is done in Go (BFS over g.Callers) rather than a CTE so
// the package stays a thin, testable layer over the graph's stable query API.
package impact

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"nilcore/internal/codeintel/graph"
)

// ImpactSet returns the symbols transitively affected by a change to `changed`:
// its transitive callers (reverse reachability), found by repeated BFS over
// g.Callers. The result is sorted, unique, and excludes `changed` itself.
func ImpactSet(ctx context.Context, g *graph.Graph, changed string) ([]string, error) {
	seen := map[string]bool{changed: true}
	queue := []string{changed}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		callers, err := g.Callers(ctx, cur)
		if err != nil {
			return nil, fmt.Errorf("callers of %q: %w", cur, err)
		}
		for _, c := range callers {
			if seen[c] {
				continue
			}
			seen[c] = true
			queue = append(queue, c)
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		if id == changed {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// AffectedTests returns the ImpactSet members whose name starts with "Test" —
// the tests that should be re-run for a change to `changed`.
func AffectedTests(ctx context.Context, g *graph.Graph, changed string) ([]string, error) {
	impacted, err := ImpactSet(ctx, g, changed)
	if err != nil {
		return nil, err
	}
	var tests []string
	for _, id := range impacted {
		if strings.HasPrefix(id, "Test") {
			tests = append(tests, id)
		}
	}
	return tests, nil
}

// Suspect is a symbol and its computed suspiciousness score.
type Suspect struct {
	Symbol string
	Score  float64
}

// Cover records how many failing and passing tests executed a symbol.
type Cover struct {
	Failed int
	Passed int
}

// Localize ranks symbols by suspiciousness using the Ochiai SBFL metric:
//
//	score = failed / sqrt(totalFailed * (failed + passed))
//
// where totalFailed is the largest Failed value in the map — i.e. a symbol
// executed by every failing test. Symbols never hit by a failing test, or any
// case that would divide by zero, score 0. Results are sorted by score
// descending, with the symbol name as a stable tie-breaker.
func Localize(coverage map[string]Cover) []Suspect {
	var totalFailed int
	for _, c := range coverage {
		if c.Failed > totalFailed {
			totalFailed = c.Failed
		}
	}
	out := make([]Suspect, 0, len(coverage))
	for sym, c := range coverage {
		out = append(out, Suspect{Symbol: sym, Score: ochiai(c.Failed, c.Passed, totalFailed)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Symbol < out[j].Symbol
	})
	return out
}

func ochiai(failed, passed, totalFailed int) float64 {
	denom := float64(totalFailed) * float64(failed+passed)
	if denom <= 0 {
		return 0
	}
	return float64(failed) / math.Sqrt(denom)
}
