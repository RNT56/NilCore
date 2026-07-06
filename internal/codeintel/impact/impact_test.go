package impact

import (
	"context"
	"os"
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

// TestSameNameNoClobberAcrossFiles is the regression that proves the qualified-id
// migration's VALUE: two files that EACH define a func named `Run`, each with its own
// caller test, must produce DISTINCT graph nodes so neither clobbers the other, and
// per-file impact must stay correct. Under the old bare-name scheme the two `Run`s
// collapsed to one node and one file's caller test was lost.
func TestSameNameNoClobberAcrossFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Two independent files, each with a func Run and a distinct test that calls it.
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte(
		"package p\nfunc Run() {}\nfunc TestRunA() { Run() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte(
		"package q\nfunc Run() {}\nfunc TestRunB() { Run() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := graph.Open(filepath.Join(dir, "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	if err := g.BuildFile(ctx, fileA); err != nil {
		t.Fatal(err)
	}
	if err := g.BuildFile(ctx, fileB); err != nil {
		t.Fatal(err)
	}

	// The two Run definitions are DISTINCT nodes (no clobber): there are exactly two
	// nodes named "Run", carrying different files and different qualified ids.
	nodes, err := g.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runIDs := map[string]string{} // qualified id -> file
	for _, n := range nodes {
		if n.Name == "Run" {
			runIDs[n.ID] = n.File
		}
	}
	if len(runIDs) != 2 {
		t.Fatalf("want 2 distinct Run nodes (one per file), got %d: %v", len(runIDs), runIDs)
	}
	files := map[string]bool{}
	for _, f := range runIDs {
		files[f] = true
	}
	if !files[fileA] || !files[fileB] {
		t.Errorf("the two Run nodes should map to a.go and b.go, got files %v", files)
	}

	// A change to Run affects BOTH tests (the bare name is ambiguous across the two
	// files, so the impact lens over-approximates to every same-named definition — the
	// documented safe behavior). Crucially, NEITHER test is lost to clobbering.
	tests, err := AffectedTests(ctx, g, "Run")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, name := range tests {
		got[name] = true
	}
	if !got["TestRunA"] || !got["TestRunB"] {
		t.Errorf("AffectedTests(Run) = %v, want both TestRunA and TestRunB (no clobber)", tests)
	}

	// The distinct definitions are what the migration guarantees; a cross-file call
	// still names the callee only by its BARE name, so a call to the ambiguous "Run"
	// resolves to callers of that name (both tests) — the documented, safe
	// over-approximation. The crucial regression: the caller edge from a.go's TestRunA
	// is NOT clobbered by indexing b.go — it survives as a distinct qualified node in
	// a.go, and each test caller resolves to the file that actually defines it.
	callersA, err := g.Callers(ctx, "Run")
	if err != nil {
		t.Fatal(err)
	}
	callerByFile := map[string]string{} // file -> caller name
	for _, id := range callersA {
		f, _, name := graph.SplitID(id)
		callerByFile[f] = name
	}
	if callerByFile[fileA] != "TestRunA" {
		t.Errorf("a.go should contribute caller TestRunA, got %q (clobbered?)", callerByFile[fileA])
	}
	if callerByFile[fileB] != "TestRunB" {
		t.Errorf("b.go should contribute caller TestRunB, got %q (clobbered?)", callerByFile[fileB])
	}
}
