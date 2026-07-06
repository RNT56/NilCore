package tools

import (
	"strings"
	"testing"
)

// TestRenderLayersNearPrefixCollision proves the layer classifier matches on a
// path-SEGMENT boundary: a prefix "mod/internal/ap" must NOT claim the package
// "mod/internal/apple", so an edge between them is not mis-reported (or wrongly
// suppressed) as a layering relationship.
func TestRenderLayersNearPrefixCollision(t *testing.T) {
	// Two sibling packages whose paths share a leading substring but not a full
	// segment. Under a bare HasPrefix match, "mod/internal/apple" would be assigned
	// the "mod/internal/ap" layer and this intra-not-a-layer edge would be judged as
	// a same-layer (or cross-layer) relationship. With a segment boundary, "apple"
	// matches no prefix, is unlayered, and the edge is skipped.
	adj := map[string]map[string]bool{
		"mod/internal/apple": {"mod/internal/ap": true},
	}
	layers := []string{"mod/internal/ap", "mod/internal/z"} // ap is HIGH, z is LOW
	out := renderLayers(adj, layers)
	if !strings.Contains(out, "0 violation(s)") {
		t.Fatalf("near-prefix collision mis-classified apple as layer ap: %q", out)
	}

	// Sanity: the real "ap" package under its own layer, importing a lower layer,
	// is still classified — the boundary must not break exact matches.
	adj2 := map[string]map[string]bool{
		"mod/internal/z/x": {"mod/internal/ap": true}, // z (low) importing ap (high) = violation
	}
	out2 := renderLayers(adj2, layers)
	if !strings.Contains(out2, "1 violation(s)") {
		t.Fatalf("segment-boundary match broke a legitimate lower→higher violation: %q", out2)
	}
	if !strings.Contains(out2, "mod/internal/z/x → mod/internal/ap") {
		t.Errorf("violation edge not reported: %q", out2)
	}
}
