// Package repomap is the PageRank repo-map (P3-T11): it ranks the nodes of the
// code graph by structural importance so the agent can spend a bounded context
// budget on the symbols that matter most. Pure text retrieval treats every file
// alike; PageRank over the directed call graph surfaces the hubs — the functions
// everything else flows through — which is exactly what a coding agent needs to
// orient in an unfamiliar repo. The ranking is deterministic (ties broken by ID)
// so the same graph always yields the same map.
package repomap

import (
	"context"
	"fmt"
	"sort"

	"nilcore/internal/codeintel/graph"
)

// PageRank parameters. Standard damping with a fixed iteration count is plenty
// for the small-to-medium graphs we map; convergence well within 30 iterations.
const (
	damping    = 0.85
	iterations = 30
)

// Entry is one ranked node of the repo-map.
type Entry struct {
	ID    string
	Kind  string
	Name  string
	File  string
	Score float64
}

// RepoMap computes PageRank over the directed `calls` graph and returns the top
// entries, highest score first (ties broken by ID, ascending), capped to budget.
// A budget <= 0 returns every node. Kind/Name/File are looked up from g.Nodes.
func RepoMap(ctx context.Context, g *graph.Graph, budget int) ([]Entry, error) {
	nodes, err := g.Nodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("repomap: load nodes: %w", err)
	}
	// CallEdges (not the raw Edges("calls")) resolves each edge's bare callee name to
	// the matching QUALIFIED node id, so endpoints line up with Nodes()'s ids. Feeding
	// the raw edges here would drop every edge in pageRank's `known[e.To]` filter (the
	// raw to_id is a bare name absent from the qualified node set), leaving PageRank
	// edge-less and every rank uniform.
	edges, err := g.CallEdges(ctx)
	if err != nil {
		return nil, fmt.Errorf("repomap: load edges: %w", err)
	}

	scores := pageRank(nodes, edges)

	entries := make([]Entry, 0, len(nodes))
	for _, n := range nodes {
		entries = append(entries, Entry{
			ID:    n.ID,
			Kind:  n.Kind,
			Name:  n.Name,
			File:  n.File,
			Score: scores[n.ID],
		})
	}

	// Highest score first; deterministic tie-break by ID so equal-rank nodes
	// (and the all-equal first iteration) never reorder between calls.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Score != entries[j].Score {
			return entries[i].Score > entries[j].Score
		}
		return entries[i].ID < entries[j].ID
	})

	if budget > 0 && budget < len(entries) {
		entries = entries[:budget]
	}
	return entries, nil
}

// pageRank runs standard iterative PageRank over the directed call graph.
// Dangling nodes (no out-edges) have their mass redistributed uniformly each
// iteration so probability is conserved; teleport is uniform across all nodes.
func pageRank(nodes []graph.Node, edges []graph.Edge) map[string]float64 {
	n := len(nodes)
	if n == 0 {
		return map[string]float64{}
	}

	// Restrict to known node IDs so a stray edge endpoint can't skew the math.
	known := make(map[string]bool, n)
	for _, nd := range nodes {
		known[nd.ID] = true
	}

	out := make(map[string][]string, n) // node -> its callees (out-links)
	outDeg := make(map[string]int, n)
	for _, e := range edges {
		if !known[e.From] || !known[e.To] {
			continue
		}
		out[e.From] = append(out[e.From], e.To)
		outDeg[e.From]++
	}

	rank := make(map[string]float64, n)
	init := 1.0 / float64(n)
	for _, nd := range nodes {
		rank[nd.ID] = init
	}

	teleport := (1.0 - damping) / float64(n)
	for i := 0; i < iterations; i++ {
		// Dangling mass: rank held by nodes with no out-edges is spread evenly
		// over the whole graph this iteration.
		var dangling float64
		for _, nd := range nodes {
			if outDeg[nd.ID] == 0 {
				dangling += rank[nd.ID]
			}
		}

		next := make(map[string]float64, n)
		base := teleport + damping*dangling/float64(n)
		for _, nd := range nodes {
			next[nd.ID] = base
		}
		for _, nd := range nodes {
			d := outDeg[nd.ID]
			if d == 0 {
				continue
			}
			share := damping * rank[nd.ID] / float64(d)
			for _, to := range out[nd.ID] {
				next[to] += share
			}
		}
		rank = next
	}
	return rank
}
