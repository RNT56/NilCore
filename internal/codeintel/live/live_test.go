package live_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"nilcore/internal/codeintel/graph"
	"nilcore/internal/codeintel/live"
	"nilcore/internal/memory"
	"nilcore/internal/store"
)

func openGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g, err := graph.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

func TestLiveIncrementalWorktreeEdit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "p.go")
	write := func(src string) {
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package p\nfunc a() {}\nfunc b() { a() }\n")

	ix := &live.Index{Graph: openGraph(t)}
	if err := ix.Update(ctx, path); err != nil {
		t.Fatal(err)
	}
	if callees, _ := ix.Graph.Callees(ctx, "b"); len(callees) != 1 || callees[0] != "a" {
		t.Fatalf("initial b callees = %v, want [a]", callees)
	}

	// Edit the file in place (an uncommitted worktree edit) and re-index it.
	write("package p\nfunc a() {}\nfunc c() {}\nfunc b() { a(); c() }\n")
	if err := ix.Update(ctx, path); err != nil {
		t.Fatal(err)
	}
	callees, _ := ix.Graph.Callees(ctx, "b")
	if len(callees) != 2 || callees[0] != "a" || callees[1] != "c" {
		t.Errorf("after edit b callees = %v, want [a c] (worktree edit reflected)", callees)
	}
}

// TestLiveRemoveDropsDeletedFile proves the deletion/rename path: after Remove,
// the gone file's symbols, its outgoing edges, AND the edges pointing into it from
// another (surviving) file are all dropped — no stale or dangling state lingers.
func TestLiveRemoveDropsDeletedFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	gonePath := filepath.Join(dir, "gone.go")
	keepPath := filepath.Join(dir, "keep.go")
	if err := os.WriteFile(gonePath, []byte("package p\nfunc Gone() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// keep.go calls Gone, so there is an edge keep:User -> Gone INTO the gone file.
	if err := os.WriteFile(keepPath, []byte("package p\nfunc User() { Gone() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ix := &live.Index{Graph: openGraph(t)}
	if err := ix.Update(ctx, gonePath); err != nil {
		t.Fatal(err)
	}
	if err := ix.Update(ctx, keepPath); err != nil {
		t.Fatal(err)
	}
	if callees, _ := ix.Graph.Callees(ctx, "User"); len(callees) != 1 || callees[0] != "Gone" {
		t.Fatalf("pre-remove User callees = %v, want [Gone]", callees)
	}

	// Delete gone.go and signal the removal.
	if err := os.Remove(gonePath); err != nil {
		t.Fatal(err)
	}
	if err := ix.Remove(ctx, gonePath); err != nil {
		t.Fatal(err)
	}

	// The Gone node is gone...
	nodes, _ := ix.Graph.Nodes(ctx)
	for _, n := range nodes {
		if n.ID == "Gone" {
			t.Errorf("removed file's symbol 'Gone' still present: %+v", n)
		}
	}
	// ...and the incoming edge from the surviving file no longer dangles.
	if callees, _ := ix.Graph.Callees(ctx, "User"); len(callees) != 0 {
		t.Errorf("post-remove User callees = %v, want [] (dangling edge into deleted file pruned)", callees)
	}
	// User itself (in the surviving file) is untouched.
	var sawUser bool
	for _, n := range nodes {
		if n.ID == "User" {
			sawUser = true
		}
	}
	if !sawUser {
		t.Error("surviving file's symbol 'User' was wrongly removed")
	}
}

func TestLiveMemoryFusion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte("package p\nfunc helper() {}\nfunc Run() { helper() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := openGraph(t)

	s, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	mem := memory.New(s)
	if err := mem.Write(ctx, memory.Record{Scope: memory.ScopeProject, Project: "p", Key: "Run", Value: "entrypoint; keep it thin"}); err != nil {
		t.Fatal(err)
	}

	ix := &live.Index{Graph: g, Memory: mem, Project: "p"}
	if err := ix.Update(ctx, path); err != nil {
		t.Fatal(err)
	}

	facts, err := ix.Query(ctx, "Run")
	if err != nil {
		t.Fatal(err)
	}
	var sawGraph, sawLead bool
	for _, f := range facts {
		if f.Provenance == "graph" && f.Symbol == "helper" {
			sawGraph = true
		}
		if f.Provenance == "lead" && f.Detail == "entrypoint; keep it thin" {
			sawLead = true
		}
	}
	if !sawGraph {
		t.Error("expected a graph fact (Run calls helper)")
	}
	if !sawLead {
		t.Error("expected a memory hit surfaced with provenance 'lead'")
	}
}
