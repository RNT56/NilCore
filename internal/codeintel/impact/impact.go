// Package impact answers "what does this change touch?" (P3-T15) — a view over the
// code graph:
//
//   - ImpactSet/AffectedTests — forward-of-blame: the transitive *callers* of a
//     changed symbol (reverse reachability). Editing a leaf ripples up to every
//     caller, so that caller set is exactly what must be re-checked; the subset
//     whose names start with "Test" is the test suite to re-run for the change.
//
// Reverse reachability is done in Go (BFS over g.Callers) rather than a CTE so
// the package stays a thin, testable layer over the graph's stable query API.
package impact

import (
	"context"
	"fmt"
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

// AffectedTests returns the ImpactSet members whose (bare) name starts with "Test" —
// the tests that should be re-run for a change to `changed`. `changed` may be a bare
// symbol name ("Target") or a qualified id; ImpactSet resolves it and returns qualified
// caller ids, so the "Test" filter runs on the bare NAME (via graph.SplitID), not the
// qualified id — the id embeds the file path, so "id starts with Test" would never hit.
// The bare test names are returned (deduped) so consumers can drive `go test -run`.
func AffectedTests(ctx context.Context, g *graph.Graph, changed string) ([]string, error) {
	impacted, err := ImpactSet(ctx, g, changed)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var tests []string
	for _, id := range impacted {
		name := graph.DisplayName(id)
		if strings.HasPrefix(name, "Test") && !seen[name] {
			seen[name] = true
			tests = append(tests, name)
		}
	}
	sort.Strings(tests)
	return tests, nil
}
