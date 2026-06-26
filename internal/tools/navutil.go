package tools

// navutil.go holds the shared, read-only host-side machinery the structural
// navigation/hygiene tools (outline, read_symbol, dead_code) build on. Everything
// here is deterministic, worktree-confined, stdlib-only AST/graph work — no
// execution, no network, no writes (I4/I6). It deliberately reuses the same
// worktree walk discipline as SearchTool/CodeintelTool (sourceFilesUnder, the
// maxIndexedFiles cap) so the cost of any one call stays bounded and reproducible.

import (
	"context"

	"nilcore/internal/codeintel/ast"
	"nilcore/internal/codeintel/graph"
)

// symRef is one declared symbol with its worktree-relative location — the unit the
// navigation tools render. It is derived directly from ast.Symbols spans (1-based
// line range), so it works for every language the AST layer supports and degrades
// to nothing (not an error) for files it cannot parse.
type symRef struct {
	Name  string
	Kind  ast.Kind
	Recv  string
	Rel   string // worktree-relative file path
	Start int
	End   int
}

// worktreeSymbols extracts every declared symbol across the worktree's source
// files, worktree-relative. It mirrors CodeintelTool's walk (sourceFilesUnder +
// the maxIndexedFiles cap) so a pathological tree can never turn one call into an
// unbounded parse. A file that does not parse contributes nothing (best-effort),
// never failing the whole call — exactly how ast.Symbols already degrades.
func worktreeSymbols(ctx context.Context, workdir string) (syms []symRef, indexed int, err error) {
	files, err := sourceFilesUnder(workdir)
	if err != nil {
		return nil, 0, err
	}
	if len(files) > maxIndexedFiles {
		files = files[:maxIndexedFiles]
	}
	for _, path := range files {
		if cerr := ctx.Err(); cerr != nil {
			return nil, indexed, cerr
		}
		ss, serr := ast.Symbols(path)
		if serr != nil || len(ss) == 0 {
			continue // unparseable / unsupported / empty: skip, never fatal
		}
		indexed++
		rel := relOrSame(workdir, path)
		for _, s := range ss {
			syms = append(syms, symRef{
				Name: s.Name, Kind: s.Kind, Recv: s.Recv,
				Rel: rel, Start: s.Span.StartLine, End: s.Span.EndLine,
			})
		}
	}
	return syms, indexed, nil
}

// buildGraph opens an EPHEMERAL in-memory call graph over the worktree and returns
// it (the caller closes it). It is the same construction CodeintelTool uses: parse
// each supported source file into graph nodes (bare-name IDs) + `calls` edges,
// best-effort, capped at maxIndexedFiles. Nothing is persisted; the graph dies with
// the caller's Close.
func buildGraph(ctx context.Context, workdir string) (g *graph.Graph, indexed int, err error) {
	g, err = graph.Open(":memory:")
	if err != nil {
		return nil, 0, err
	}
	files, err := sourceFilesUnder(workdir)
	if err != nil {
		g.Close()
		return nil, 0, err
	}
	if len(files) > maxIndexedFiles {
		files = files[:maxIndexedFiles]
	}
	for _, path := range files {
		if cerr := ctx.Err(); cerr != nil {
			g.Close()
			return nil, indexed, cerr
		}
		if berr := g.BuildFile(ctx, path); berr != nil {
			continue
		}
		indexed++
	}
	return g, indexed, nil
}

// forwardReachable returns the set of node IDs reachable from any root by following
// `calls` edges forward (a callee BFS) — the mirror of impact.ImpactSet, which
// walks callers. The roots themselves are included. It is the reachability core of
// the dead-code lens.
func forwardReachable(ctx context.Context, g *graph.Graph, roots []string) (map[string]bool, error) {
	seen := make(map[string]bool, len(roots))
	queue := make([]string, 0, len(roots))
	for _, r := range roots {
		if !seen[r] {
			seen[r] = true
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		callees, err := g.Callees(ctx, cur)
		if err != nil {
			return nil, err
		}
		for _, c := range callees {
			if !seen[c] {
				seen[c] = true
				queue = append(queue, c)
			}
		}
	}
	return seen, nil
}

// isExportedName reports whether a symbol name is exported in the Go sense (leading
// upper-case rune). Used to seed the dead-code root set (an exported symbol is a
// public-API entry point a name-only graph cannot prove unused) and to annotate
// outline rows.
func isExportedName(name string) bool {
	if name == "" {
		return false
	}
	r := []rune(name)[0]
	return r >= 'A' && r <= 'Z'
}
