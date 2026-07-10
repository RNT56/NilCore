// Package live keeps code intelligence current cheaply and fuses it with memory
// (P3-T16): on a file change it incrementally re-indexes just that file into the
// graph (no full re-index), and because it reads the file as-is it is worktree-
// aware — the agent's own uncommitted edits are reflected immediately. Queries
// return static graph facts fused with cross-project memory hits, with memory
// surfaced as provenance "lead".
package live

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"

	"nilcore/internal/codeintel/ast"
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

// Remove drops a deleted or renamed-away file from the live graph (idempotent;
// only that file's nodes/edges, including edges pointing into its symbols from
// elsewhere, are touched). Update keeps a file's incoming edges because the file
// still exists; Remove is for a path that is gone, so its symbols' incoming edges
// would otherwise dangle. The caller signals deletes/renames it observes (the
// structured delete path); a missing path is a clean no-op.
func (ix *Index) Remove(ctx context.Context, path string) error {
	return ix.Graph.RemoveFile(ctx, path)
}

// indexSkipDir reports whether a directory NAME (never the walk root itself — the
// caller guards that) should be pruned when seeding the graph: a dependency/build
// tree (node_modules, vendor, dist, build, __pycache__) or any hidden dir (a leading
// ".", which covers .git). Mirrors the tools-package indexer + cmd/nilcore/repomap.go
// so the live graph reflects the project's own source, not vendored dependencies.
func indexSkipDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "__pycache__", "dist", "build":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// IndexDir seeds the graph from every supported-language source file under dir
// (the initial state a fresh run needs before Update keeps it current
// incrementally). Supported extensions come from ast.SupportedExtensions (all 19
// languages / 34 extensions the parser seam ships, not Go alone). Best-effort: a
// file that does not parse is skipped; a symlink is NEVER followed (I4: a link
// planted in-tree could otherwise leak out-of-worktree file content when parsed
// host-side); dependency/VCS/hidden dirs are pruned; and the walk is the only full
// pass — thereafter Update touches one file at a time (P3-T16's "no full re-index").
func (ix *Index) IndexDir(ctx context.Context, dir string) error {
	supported := map[string]bool{}
	for _, e := range ast.SupportedExtensions() {
		supported[strings.ToLower(e)] = true
	}
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Prune dependency/VCS/hidden dirs — but never the walk root itself (its
			// base name might coincidentally match, which would skip the whole tree).
			if path != dir && indexSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		// Never follow a symlink (I4): WalkDir yields it as a non-dir entry without
		// descending, but BuildFile would os.ReadFile through it — a planted link
		// could leak out-of-worktree content. Skip it.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if supported[strings.ToLower(filepath.Ext(d.Name()))] {
			_ = ix.Graph.BuildFile(ctx, path) // best-effort: a non-parsing file is skipped
		}
		return nil
	})
}

// Fact is one fused result.
type Fact struct {
	Symbol     string
	Provenance string // "graph" | "lead" (a memory hit)
	Detail     string
}

// Query returns the graph neighborhood of symbol fused with memory hits. Memory
// is surfaced with provenance "lead" (it points where to look), alongside the
// structural graph facts. Callees/Callers return QUALIFIED node ids (the same-name
// collision fix), so each is projected to its human-facing DISPLAY name (bare name,
// or "recv.name" for a method) via graph.DisplayName — the Fact and its Detail never
// leak the file path or NUL delimiters a qualified id carries.
func (ix *Index) Query(ctx context.Context, symbol string) ([]Fact, error) {
	var facts []Fact
	// The queried symbol is displayed by its own bare name too (the caller may pass a
	// qualified id or a bare name; normalize for the human-readable Detail).
	symName := graph.DisplayName(symbol)

	callees, err := ix.Graph.Callees(ctx, symbol)
	if err != nil {
		return nil, err
	}
	for _, c := range callees {
		name := graph.DisplayName(c)
		facts = append(facts, Fact{Symbol: name, Provenance: "graph", Detail: symName + " calls " + name})
	}
	callers, err := ix.Graph.Callers(ctx, symbol)
	if err != nil {
		return nil, err
	}
	for _, c := range callers {
		name := graph.DisplayName(c)
		facts = append(facts, Fact{Symbol: name, Provenance: "graph", Detail: name + " calls " + symName})
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
