package graph

import (
	"context"
	"os"
	"path/filepath"
	"sort"
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
	// Callees now returns QUALIFIED ids (the same-name-collision fix); resolve the bare
	// callee name from the returned id so the behavioral assertion ("Run calls helper,
	// deduped") is unchanged.
	callees, _ := g.Callees(ctx, "Run")
	if names := nodeNames(callees); len(names) != 1 || names[0] != "helper" {
		t.Errorf("Run callees = %v (names %v), want one 'helper' (deduped)", callees, names)
	}
}

// nodeNames extracts the bare name from each qualified node id (NodeID = file NUL
// recv NUL name), sorted, so a behavioral assertion about WHICH symbols are involved
// stays independent of the qualified-id encoding.
func nodeNames(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		_, _, name := SplitID(id)
		out = append(out, name)
	}
	sort.Strings(out)
	return out
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
	if callees, _ := g.Callees(ctx, "Run"); len(nodeNames(callees)) != 1 || nodeNames(callees)[0] != "helper" {
		t.Fatalf("v1 Run callees = %v, want one 'helper'", callees)
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
		if n.Name == "helper" {
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

// TestRemoveFilePrunesNodesAndIncomingEdges proves RemoveFile — the delete/rename
// counterpart to BuildFile — drops a gone file's symbol nodes AND the dangling edges
// pointing INTO them from a surviving file (the one thing BuildFile deliberately does
// NOT do). This is the guarantee the native-loop delete/rename wiring relies on.
func TestRemoveFilePrunesNodesAndIncomingEdges(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone.go")
	keep := filepath.Join(dir, "keep.go")
	g := openMem(t)
	ctx := context.Background()

	if err := os.WriteFile(gone, []byte("package p\nfunc Gone() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// keep.go's User calls Gone, so there is an incoming `calls` edge into gone.go.
	if err := os.WriteFile(keep, []byte("package p\nfunc User() { Gone() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.BuildFile(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if err := g.BuildFile(ctx, keep); err != nil {
		t.Fatal(err)
	}
	if callees, _ := g.Callees(ctx, "User"); len(nodeNames(callees)) != 1 || nodeNames(callees)[0] != "Gone" {
		t.Fatalf("pre-remove User callees = %v, want [Gone]", nodeNames(callees))
	}

	// Remove gone.go: its node drops and the incoming edge no longer dangles.
	if err := g.RemoveFile(ctx, gone); err != nil {
		t.Fatal(err)
	}
	nodes, _ := g.Nodes(ctx)
	for _, n := range nodes {
		if n.Name == "Gone" {
			t.Errorf("removed file's symbol 'Gone' still present: %+v", n)
		}
	}
	if callees, _ := g.Callees(ctx, "User"); len(callees) != 0 {
		t.Errorf("post-remove User callees = %v, want [] (dangling in-edge pruned)", nodeNames(callees))
	}
	// The surviving file's own symbol is untouched.
	var sawUser bool
	for _, n := range nodes {
		if n.Name == "User" {
			sawUser = true
		}
	}
	if !sawUser {
		t.Error("surviving file's symbol 'User' was wrongly removed")
	}
}

// TestRemoveFileKeepsBareNameEdgeStillLive is the survivor guard: a bare-name
// incoming `calls` edge must NOT be pruned when ANOTHER file still defines that name,
// or the removal would break a call relationship that is still live for the survivor.
func TestRemoveFileKeepsBareNameEdgeStillLive(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	caller := filepath.Join(dir, "caller.go")
	g := openMem(t)
	ctx := context.Background()

	// Both a.go and b.go define Dup; caller.go calls Dup (a bare-name edge that fans
	// out to both definitions).
	if err := os.WriteFile(a, []byte("package p\nfunc Dup() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("package q\nfunc Dup() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caller, []byte("package p\nfunc C() { Dup() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{a, b, caller} {
		if err := g.BuildFile(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	// Remove a.go. b.go still defines Dup, so the caller's bare-name edge must stay.
	if err := g.RemoveFile(ctx, a); err != nil {
		t.Fatal(err)
	}
	callees, _ := g.Callees(ctx, "C")
	if got := nodeNames(callees); len(got) != 1 || got[0] != "Dup" {
		t.Errorf("post-remove C callees = %v, want [Dup] (edge still live via the survivor)", got)
	}
	// And the survivor's Dup node is the one that resolves.
	for _, id := range callees {
		file, _, _ := SplitID(id)
		if file == a {
			t.Errorf("callee resolved to the REMOVED file %q: %q", a, id)
		}
	}
}
