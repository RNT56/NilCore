package impact

import (
	"context"
	"path/filepath"
	"testing"

	"nilcore/internal/codeintel/graph"
)

// seed builds the call edges: a -> b -> c, plus a test TestB -> b.
func seed(t *testing.T) *graph.Graph {
	t.Helper()
	g, err := graph.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := g.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	for _, e := range []graph.Edge{
		{From: "a", To: "b", Kind: "calls"},
		{From: "b", To: "c", Kind: "calls"},
		{From: "TestB", To: "b", Kind: "calls"},
	} {
		if err := g.AddEdge(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestImpactSet(t *testing.T) {
	g := seed(t)
	ctx := context.Background()

	// Changing c ripples up to its transitive callers: b (direct caller), then
	// a and TestB (both call b, so both transitively reach c). The changed
	// symbol c itself is excluded.
	impC, err := ImpactSet(ctx, g, "c")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(impC, "b") || !contains(impC, "a") {
		t.Errorf("ImpactSet(c) = %v, want to contain a and b", impC)
	}
	if contains(impC, "c") {
		t.Errorf("ImpactSet(c) = %v, must exclude changed symbol c", impC)
	}

	// Changing b ripples to a (a->b) and TestB (TestB->b).
	impB, err := ImpactSet(ctx, g, "b")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(impB, "a") {
		t.Errorf("ImpactSet(b) = %v, want to contain a", impB)
	}
	if !contains(impB, "TestB") {
		t.Errorf("ImpactSet(b) = %v, want to contain TestB", impB)
	}

	// Result must be sorted.
	for i := 1; i < len(impB); i++ {
		if impB[i-1] > impB[i] {
			t.Errorf("ImpactSet(b) = %v, not sorted", impB)
			break
		}
	}
}

func TestAffectedTests(t *testing.T) {
	g := seed(t)
	ctx := context.Background()

	tests, err := AffectedTests(ctx, g, "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(tests) != 1 || tests[0] != "TestB" {
		t.Errorf("AffectedTests(b) = %v, want [TestB]", tests)
	}
}

func TestLocalize(t *testing.T) {
	// "buggy" is hit by both failing tests and no passing test; "noise" is hit
	// by one failing and three passing tests. Ochiai must rank buggy first.
	suspects := Localize(map[string]Cover{
		"buggy": {Failed: 2, Passed: 0},
		"noise": {Failed: 1, Passed: 3},
	})
	if len(suspects) != 2 {
		t.Fatalf("Localize returned %d suspects, want 2", len(suspects))
	}
	if suspects[0].Symbol != "buggy" {
		t.Errorf("top suspect = %q, want buggy (ranked: %+v)", suspects[0].Symbol, suspects)
	}
	if suspects[0].Score <= suspects[1].Score {
		t.Errorf("buggy score %v should exceed noise score %v", suspects[0].Score, suspects[1].Score)
	}
}

func TestLocalizeTieBreakAndZero(t *testing.T) {
	// Equal scores tie-break by symbol name; a symbol with no failures scores 0.
	suspects := Localize(map[string]Cover{
		"zzz":   {Failed: 1, Passed: 0},
		"aaa":   {Failed: 1, Passed: 0},
		"clean": {Failed: 0, Passed: 5},
	})
	if suspects[0].Symbol != "aaa" || suspects[1].Symbol != "zzz" {
		t.Errorf("tie-break order = %q,%q, want aaa,zzz", suspects[0].Symbol, suspects[1].Symbol)
	}
	last := suspects[len(suspects)-1]
	if last.Symbol != "clean" || last.Score != 0 {
		t.Errorf("clean suspect = %+v, want score 0 and last", last)
	}
}

func TestLocalizeEmpty(t *testing.T) {
	if got := Localize(map[string]Cover{}); len(got) != 0 {
		t.Errorf("Localize(empty) = %v, want empty", got)
	}
}
