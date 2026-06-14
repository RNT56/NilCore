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
