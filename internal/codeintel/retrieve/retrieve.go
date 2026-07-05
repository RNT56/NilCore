// Package retrieve is the fusion layer of code intelligence (P3-T14): it routes a
// need through the lenses (semantic entry points → graph-expanded neighborhood →
// PageRank orientation) and assembles a Context Bundle — minimal-sufficient,
// structurally coherent, budget-bounded, with provenance and a rationale on every
// item. The Bundle is the unit handed to the loop, so it stays deterministic
// under fixed inputs.
package retrieve

import (
	"context"
	"sort"
	"strings"

	"nilcore/internal/codeintel/graph"
	"nilcore/internal/codeintel/lsp"
	"nilcore/internal/codeintel/repomap"
	"nilcore/internal/codeintel/semantic"
)

// Item is one element of a Context Bundle, with why it was included.
//
// Symbol is the human-facing DISPLAY name (a bare symbol name, or "recv.name" for a
// method) — safe to render, never leaking the file path or NUL delimiters a qualified
// graph id carries. ID is the retrieval IDENTITY: the qualified graph node id when the
// item comes from a graph node (so two same-named symbols in different files stay
// distinct and dedup does not collapse them), else the same bare name as Symbol. Dedup
// and any cross-item wiring key on ID; only Symbol is shown.
type Item struct {
	ID         string
	Symbol     string
	File       string
	Provenance string // "precise" | "semantic" | "lexical" | "graph-neighbor" | "repomap"
	Rationale  string
	Score      float64
}

// Bundle is the structurally-coherent context selected for a need.
type Bundle struct {
	Need  string
	Items []Item
}

// Retriever fuses the lenses. Semantic is optional (lexical fallback over the
// graph's node names when absent). LSP is optional too: when wired, a language
// server contributes compiler-grade "precise" symbol matches; when nil, retrieval
// degrades to the graph-native lenses (byte-identical to before).
type Retriever struct {
	Graph    *graph.Graph
	Semantic *semantic.Index
	LSP      *lsp.Client
}

// provRank orders provenances when scores tie (most-relevant lens first). "precise"
// (language-server, compiler-grade) ranks above the heuristic lenses.
var provRank = map[string]int{"precise": -1, "semantic": 0, "lexical": 1, "graph-neighbor": 2, "repomap": 3}

// Retrieve assembles a Context Bundle for need, bounded to budget items.
func (r *Retriever) Retrieve(ctx context.Context, need string, budget int) (Bundle, error) {
	b := Bundle{Need: need}
	seen := map[string]bool{}
	// add records one item, keyed for dedup on its IDENTITY (id — a qualified graph
	// node id, so two same-named symbols in different files do NOT collapse), while the
	// rendered Symbol is the human-facing DISPLAY name derived from that id. Graph ids
	// carry the file path + NUL delimiters, so they must never be shown; graph.DisplayName
	// is the display-side projection.
	add := func(id, file, prov, why string, score float64) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		b.Items = append(b.Items, Item{
			ID:         id,
			Symbol:     graph.DisplayName(id),
			File:       file,
			Provenance: prov,
			Rationale:  why,
			Score:      score,
		})
	}

	nodes, err := r.Graph.Nodes(ctx)
	if err != nil {
		return b, err
	}
	fileOf := make(map[string]string, len(nodes))
	// byName maps a bare symbol NAME to the qualified node(s) that carry it. The
	// semantic index is keyed by bare symbol name (see internal/tools.openSemantic),
	// but graph nodes are keyed by QUALIFIED id, so a semantic hit must be resolved
	// through byName to land in the graph's id space (a name shared by two files
	// resolves to both nodes — the same over-approximation the graph query layer makes).
	byName := make(map[string][]graph.Node, len(nodes))
	for _, n := range nodes {
		fileOf[n.ID] = n.File
		byName[n.Name] = append(byName[n.Name], n)
	}

	// 1a. Precise entry points: a language server's global symbol search — exact,
	// cross-language, compiler-grade. Best-effort: a server error degrades silently
	// to the heuristic lenses below. Precise hits are standalone items (their names
	// need not be graph node ids), ranked highest.
	if r.LSP != nil {
		if hits, lerr := r.LSP.Symbol(ctx, need); lerr == nil {
			for _, h := range hits {
				add(h.Name, uriToPath(h.Location.URI), "precise", "language-server symbol match", 1.5)
			}
		}
	}

	// 1b. Entry points: semantic search, else lexical over node names.
	//
	// The semantic index is PERSISTENT across runs, so a renamed/deleted symbol's
	// row can linger and Search can return a name absent from the current tree. The
	// graph, by contrast, is rebuilt fresh from the live source, so its node set is
	// ground truth. A hit's id is a bare symbol NAME (the semantic index's key); it is
	// resolved through byName to the qualified graph node(s). A hit that resolves to no
	// node is a phantom (a stale index row with no live symbol): including it would
	// render dead code as current, so it is dropped (the correctness guard); the index
	// itself is separately reconciled at build time so it does not grow unbounded.
	var leads []string
	if r.Semantic != nil {
		if hits, serr := r.Semantic.Search(ctx, need, 5); serr == nil {
			for _, h := range hits {
				matches := byName[h.ID]
				if len(matches) == 0 {
					continue // phantom: stale index row with no live graph node
				}
				for _, n := range matches {
					leads = append(leads, n.ID)
					add(n.ID, n.File, "semantic", "matches the query", h.Score)
				}
			}
		}
	}
	if len(leads) == 0 {
		for _, t := range strings.Fields(strings.ToLower(need)) {
			for _, n := range nodes {
				if strings.Contains(strings.ToLower(n.Name), t) {
					leads = append(leads, n.ID)
					add(n.ID, n.File, "lexical", "name matches the query", 1)
				}
			}
		}
	}

	// 2. Expand each lead by its immediate neighborhood — structurally coherent. The
	// rationale names the lead by its DISPLAY name (graph.DisplayName), never the raw
	// qualified id, which would leak the file path into the human-readable text.
	for _, lead := range leads {
		leadName := graph.DisplayName(lead)
		callees, _ := r.Graph.Callees(ctx, lead)
		for _, c := range callees {
			add(c, fileOf[c], "graph-neighbor", "called by "+leadName, 0.5)
		}
		callers, _ := r.Graph.Callers(ctx, lead)
		for _, c := range callers {
			add(c, fileOf[c], "graph-neighbor", "calls "+leadName, 0.5)
		}
	}

	// 3. Orientation: a few central hubs.
	if hubs, herr := repomap.RepoMap(ctx, r.Graph, 3); herr == nil {
		for _, h := range hubs {
			add(h.ID, h.File, "repomap", "central to the repo", h.Score)
		}
	}

	// 4. Deterministic order (score desc, then lens, then symbol) and budget cap.
	sort.SliceStable(b.Items, func(i, j int) bool {
		if b.Items[i].Score != b.Items[j].Score {
			return b.Items[i].Score > b.Items[j].Score
		}
		if provRank[b.Items[i].Provenance] != provRank[b.Items[j].Provenance] {
			return provRank[b.Items[i].Provenance] < provRank[b.Items[j].Provenance]
		}
		return b.Items[i].Symbol < b.Items[j].Symbol
	})
	if budget > 0 && budget < len(b.Items) {
		b.Items = b.Items[:budget]
	}
	return b, nil
}

// uriToPath strips a file:// scheme from an LSP URI so a precise item carries the
// same plain path shape as the graph-derived items (the renderer makes it
// worktree-relative). A non-file URI is returned unchanged.
func uriToPath(uri string) string {
	return strings.TrimPrefix(uri, "file://")
}
