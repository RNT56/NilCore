// Package live keeps code intelligence current cheaply and fuses it with memory
// (P3-T16): on a file change it incrementally re-indexes just that file into the
// graph (no full re-index), and because it reads the file as-is it is worktree-
// aware — the agent's own uncommitted edits are reflected immediately. Queries
// return static graph facts fused with cross-project memory hits, with memory
// surfaced as provenance "lead".
package live

import (
	"context"

	"nilcore/internal/codeintel/graph"
	"nilcore/internal/memory"
)

// Index fuses the live code graph with project memory.
type Index struct {
	Graph   *graph.Graph
	Memory  *memory.Memory // optional
	Project string
}

// Update incrementally re-indexes a single changed file (idempotent; only that
// file's nodes/edges are touched). Worktree-aware: uncommitted edits are picked
// up because the file is read directly.
func (ix *Index) Update(ctx context.Context, path string) error {
	return ix.Graph.BuildFile(ctx, path)
}

// Fact is one fused result.
type Fact struct {
	Symbol     string
	Provenance string // "graph" | "lead" (a memory hit)
	Detail     string
}

// Query returns the graph neighborhood of symbol fused with memory hits. Memory
// is surfaced with provenance "lead" (it points where to look), alongside the
// structural graph facts.
func (ix *Index) Query(ctx context.Context, symbol string) ([]Fact, error) {
	var facts []Fact

	callees, err := ix.Graph.Callees(ctx, symbol)
	if err != nil {
		return nil, err
	}
	for _, c := range callees {
		facts = append(facts, Fact{Symbol: c, Provenance: "graph", Detail: symbol + " calls " + c})
	}
	callers, err := ix.Graph.Callers(ctx, symbol)
	if err != nil {
		return nil, err
	}
	for _, c := range callers {
		facts = append(facts, Fact{Symbol: c, Provenance: "graph", Detail: c + " calls " + symbol})
	}

	if ix.Memory != nil {
		recs, err := ix.Memory.Query(ctx, memory.ScopeProject, ix.Project, symbol)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			facts = append(facts, Fact{Symbol: r.Key, Provenance: "lead", Detail: r.Value})
		}
	}
	return facts, nil
}
