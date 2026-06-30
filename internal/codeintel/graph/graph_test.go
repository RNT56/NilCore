package graph

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func openMem(t *testing.T) *Graph {
	t.Helper()
	g, err := Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

// edges: a -> b -> c, and a -> d
func seed(t *testing.T, g *Graph) {
	ctx := context.Background()
	for _, e := range []Edge{
		{"a", "b", "calls"}, {"b", "c", "calls"}, {"a", "d", "calls"},
	} {
		if err := g.AddEdge(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCallersCallees(t *testing.T) {
	g := openMem(t)
	seed(t, g)
	ctx := context.Background()

	callees, _ := g.Callees(ctx, "a")
	if len(callees) != 2 || callees[0] != "b" || callees[1] != "d" {
		t.Errorf("Callees(a) = %v, want [b d]", callees)
	}
	callers, _ := g.Callers(ctx, "c")
	if len(callers) != 1 || callers[0] != "b" {
		t.Errorf("Callers(c) = %v, want [b]", callers)
	}
}

func TestClosureAndReachable(t *testing.T) {
	g := openMem(t)
	seed(t, g)
	ctx := context.Background()

	clos, _ := g.Closure(ctx, "a")
	want := map[string]bool{"b": true, "c": true, "d": true}
	if len(clos) != 3 {
		t.Fatalf("Closure(a) = %v, want 3 nodes", clos)
	}
	for _, id := range clos {
		if !want[id] {
			t.Errorf("unexpected node %q in closure", id)
		}
	}
	if ok, _ := g.Reachable(ctx, "a", "c"); !ok {
		t.Error("c should be reachable from a (a->b->c)")
	}
	if ok, _ := g.Reachable(ctx, "c", "a"); ok {
		t.Error("a should not be reachable from c")
	}
}

func TestBuildFileIdempotent(t *testing.T) {
	src := `package p
func helper() int { return 1 }
func Run() int { return helper() + helper() }
`
	path := filepath.Join(t.TempDir(), "p.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	g := openMem(t)
	ctx := context.Background()
	if err := g.BuildFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	if err := g.BuildFile(ctx, path); err != nil { // rebuild must not duplicate
		t.Fatal(err)
	}
	callees, _ := g.Callees(ctx, "Run")
	if len(callees) != 1 || callees[0] != "helper" {
		t.Errorf("Run callees = %v, want [helper] (deduped)", callees)
	}
}

// TestBuildFilePrunesRemovedSymbols is the regression for the additive-only bug:
// re-indexing a file after an edit that DELETES a symbol must drop that symbol's
// node and its outgoing edges, not leave them lingering — the incremental `live`
// re-index relies on this to reflect the agent's own edits.
func TestBuildFilePrunesRemovedSymbols(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.go")
	g := openMem(t)
	ctx := context.Background()

	// v1: Run calls helper.
	if err := os.WriteFile(path, []byte("package p\nfunc helper() int { return 1 }\nfunc Run() int { return helper() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.BuildFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	if callees, _ := g.Callees(ctx, "Run"); len(callees) != 1 || callees[0] != "helper" {
		t.Fatalf("v1 Run callees = %v, want [helper]", callees)
	}

	// v2: helper deleted, Run no longer calls it.
	if err := os.WriteFile(path, []byte("package p\nfunc Run() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.BuildFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	if callees, _ := g.Callees(ctx, "Run"); len(callees) != 0 {
		t.Errorf("after re-index, Run callees = %v, want [] (stale call pruned)", callees)
	}
	nodes, _ := g.Nodes(ctx)
	for _, n := range nodes {
		if n.ID == "helper" {
			t.Errorf("deleted symbol 'helper' still present after re-index: %+v", n)
		}
	}
}

// TestBuildFileReferencesEdges proves BuildFile emits `references` edges (the tag
// map: file -> each identifier it uses) and prunes them on rebuild, so a reference
// the file no longer contains does not linger.
func TestBuildFileReferencesEdges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.go")
	g := openMem(t)
	ctx := context.Background()

	// v1: Run calls helper — both names appear as references.
	if err := os.WriteFile(path, []byte("package p\nfunc helper() int { return 1 }\nfunc Run() int { return helper() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.BuildFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	refTo := func() map[string]bool {
		edges, err := g.Edges(ctx, "references")
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]bool{}
		for _, e := range edges {
			if e.From != path {
				t.Errorf("reference edge from %q, want the file path %q", e.From, path)
			}
			m[e.To] = true
		}
		return m
	}
	if got := refTo(); !got["helper"] {
		t.Errorf("v1 references = %v, want a reference to helper", got)
	}

	// v2: helper removed; its reference edge must be pruned on rebuild.
	if err := os.WriteFile(path, []byte("package p\nfunc Run() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.BuildFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	if got := refTo(); got["helper"] {
		t.Errorf("after re-index, references still include helper: %v (stale reference not pruned)", got)
	}
}
