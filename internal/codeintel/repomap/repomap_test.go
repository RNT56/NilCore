package repomap

import (
	"context"
	"path/filepath"
	"testing"

	"nilcore/internal/codeintel/graph"
)

// buildGraph opens an ephemeral graph and seeds the given nodes/edges.
func buildGraph(t *testing.T, nodes []graph.Node, edges []graph.Edge) *graph.Graph {
	t.Helper()
	g, err := graph.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := g.Close(); err != nil {
			t.Errorf("close graph: %v", err)
		}
	})
	ctx := context.Background()
	for _, n := range nodes {
		if err := g.AddNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range edges {
		if err := g.AddEdge(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// seedHub builds a graph where "b" is the most-called node:
// a->b, c->b, a->c. So b has two incoming calls and should rank at the top.
func seedHub(t *testing.T) *graph.Graph {
	t.Helper()
	nodes := []graph.Node{
		{ID: "a", Kind: "func", Name: "a", File: "x.go"},
		{ID: "b", Kind: "func", Name: "b", File: "x.go"},
		{ID: "c", Kind: "func", Name: "c", File: "x.go"},
	}
	edges := []graph.Edge{
		{From: "a", To: "b", Kind: "calls"},
		{From: "c", To: "b", Kind: "calls"},
		{From: "a", To: "c", Kind: "calls"},
	}
	return buildGraph(t, nodes, edges)
}

func TestRepoMapRanksHubTop(t *testing.T) {
	g := seedHub(t)
	ctx := context.Background()

	entries, err := RepoMap(ctx, g, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("RepoMap returned %d entries, want 3", len(entries))
	}
	if entries[0].ID != "b" {
		t.Errorf("top entry = %q (score %v), want b; full ranking = %v",
			entries[0].ID, entries[0].Score, ids(entries))
	}
	// b is the only node with two callers, so it must out-rank both others.
	for _, e := range entries[1:] {
		if e.Score > entries[0].Score {
			t.Errorf("node %q (%v) out-ranks hub b (%v)", e.ID, e.Score, entries[0].Score)
		}
	}
	// Carried-through metadata must come from g.Nodes.
	if entries[0].Kind != "func" || entries[0].Name != "b" || entries[0].File != "x.go" {
		t.Errorf("top entry metadata = %+v, want kind=func name=b file=x.go", entries[0])
	}
}

// TestRepoMapResolvesQualifiedEdges is the regression guard for the qualified-id
// migration: real BuildFile stores nodes under QUALIFIED ids (NodeID(file,recv,name))
// while a `calls` edge's to_id is the BARE callee name. RepoMap must consume CallEdges
// (which resolves the bare name to the qualified node) — feeding it the raw Edges("calls")
// drops every edge (the bare to_id is absent from the qualified node set), leaving
// PageRank edge-less and every rank uniform. Here the hub (two incoming calls) must still
// STRICTLY out-rank the others. (seedHub uses bare ids where both edge forms coincide, so
// it cannot catch this — this test uses the real keying.)
func TestRepoMapResolvesQualifiedEdges(t *testing.T) {
	aID := graph.NodeID("x.go", "", "a")
	bID := graph.NodeID("x.go", "", "b")
	cID := graph.NodeID("x.go", "", "c")
	nodes := []graph.Node{
		{ID: aID, Kind: "func", Name: "a", File: "x.go"},
		{ID: bID, Kind: "func", Name: "b", File: "x.go"},
		{ID: cID, Kind: "func", Name: "c", File: "x.go"},
	}
	// To carries the BARE callee name, exactly as BuildFile records a call edge.
	edges := []graph.Edge{
		{From: aID, To: "b", Kind: "calls"},
		{From: cID, To: "b", Kind: "calls"},
		{From: aID, To: "c", Kind: "calls"},
	}
	g := buildGraph(t, nodes, edges)

	entries, err := RepoMap(context.Background(), g, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("RepoMap returned %d entries, want 3", len(entries))
	}
	if entries[0].ID != bID {
		t.Errorf("top entry = %q, want the hub %q — call edges were dropped (uniform ranks), i.e. raw Edges not CallEdges. full = %v",
			entries[0].ID, bID, ids(entries))
	}
	if entries[0].Score <= entries[len(entries)-1].Score {
		t.Errorf("hub score %v not strictly above the lowest %v — resolved call edges did not drive PageRank",
			entries[0].Score, entries[len(entries)-1].Score)
	}
}

func TestRepoMapBudgetCaps(t *testing.T) {
	g := seedHub(t)
	ctx := context.Background()

	entries, err := RepoMap(ctx, g, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("budget=2 returned %d entries, want 2", len(entries))
	}
	if entries[0].ID != "b" {
		t.Errorf("budgeted top = %q, want b", entries[0].ID)
	}
}

func TestRepoMapDeterministic(t *testing.T) {
	g := seedHub(t)
	ctx := context.Background()

	first, err := RepoMap(ctx, g, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RepoMap(ctx, g, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("length differs across calls: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("entry %d differs across calls: %+v vs %+v", i, first[i], second[i])
		}
	}
}

func TestRepoMapEmpty(t *testing.T) {
	g := buildGraph(t, nil, nil)
	ctx := context.Background()

	entries, err := RepoMap(ctx, g, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("empty graph returned %d entries, want 0", len(entries))
	}
}

// TestRepoMapTieBreak verifies equal-score nodes order by ID ascending so the
// output is stable. With no edges every node shares the teleport score.
func TestRepoMapTieBreak(t *testing.T) {
	nodes := []graph.Node{
		{ID: "z", Kind: "func", Name: "z", File: "x.go"},
		{ID: "m", Kind: "func", Name: "m", File: "x.go"},
		{ID: "a", Kind: "func", Name: "a", File: "x.go"},
	}
	g := buildGraph(t, nodes, nil)
	ctx := context.Background()

	entries, err := RepoMap(ctx, g, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := ids(entries)
	want := []string{"a", "m", "z"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tie-break order = %v, want %v", got, want)
		}
	}
}

func ids(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}
