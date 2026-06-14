package retrieve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"nilcore/internal/codeintel/graph"
	"nilcore/internal/codeintel/semantic"
)

func buildFixture(t *testing.T) *Retriever {
	t.Helper()
	ctx := context.Background()
	src := `package p
func leaf() int { return 1 }
func helper() int { return leaf() }
func Run() int { return helper() }
`
	path := filepath.Join(t.TempDir(), "p.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := graph.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	if err := g.BuildFile(ctx, path); err != nil {
		t.Fatal(err)
	}

	sem, err := semantic.Open(filepath.Join(t.TempDir(), "s.db"), nil) // lexical
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sem.Close() })
	for id, text := range map[string]string{
		"Run":    "Run the program entry point",
		"helper": "helper utility",
		"leaf":   "leaf computation",
	} {
		if err := sem.Add(ctx, id, text); err != nil {
			t.Fatal(err)
		}
	}
	return &Retriever{Graph: g, Semantic: sem}
}

func TestRetrieveBundle(t *testing.T) {
	r := buildFixture(t)
	b, err := r.Retrieve(context.Background(), "Run", 10)
	if err != nil {
		t.Fatal(err)
	}

	byID := map[string]Item{}
	for _, it := range b.Items {
		byID[it.Symbol] = it
		if it.Rationale == "" || it.Provenance == "" {
			t.Errorf("item %q missing provenance/rationale: %+v", it.Symbol, it)
		}
	}
	// The lead is present...
	if _, ok := byID["Run"]; !ok {
		t.Error("bundle should include the lead Run")
	}
	// ...and its immediate neighborhood (Run calls helper) — structurally coherent.
	if it, ok := byID["helper"]; !ok || it.Provenance != "graph-neighbor" {
		t.Errorf("bundle should include helper as a graph-neighbor; got %+v", it)
	}
}

func TestRetrieveBudget(t *testing.T) {
	r := buildFixture(t)
	b, err := r.Retrieve(context.Background(), "Run", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Items) > 2 {
		t.Errorf("bundle exceeded budget: %d items", len(b.Items))
	}
}

func TestRetrieveDeterministic(t *testing.T) {
	r := buildFixture(t)
	ctx := context.Background()
	b1, _ := r.Retrieve(ctx, "Run", 10)
	b2, _ := r.Retrieve(ctx, "Run", 10)
	if len(b1.Items) != len(b2.Items) {
		t.Fatalf("nondeterministic length: %d vs %d", len(b1.Items), len(b2.Items))
	}
	for i := range b1.Items {
		if b1.Items[i].Symbol != b2.Items[i].Symbol {
			t.Errorf("nondeterministic order at %d: %q vs %q", i, b1.Items[i].Symbol, b2.Items[i].Symbol)
		}
	}
}
