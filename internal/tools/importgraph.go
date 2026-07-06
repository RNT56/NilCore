package tools

// importgraph.go — a read-only package dependency lens for Go modules. It converts
// a NAMED architectural rule that prose + manual review enforce today ("leaf
// packages must not import the orchestrator … keep the core acyclic") into a
// structured, stdlib-only check `make verify` cannot give you: import cycles and
// layering-direction violations. Pure go/parser (ImportsOnly) + a stdlib Tarjan
// SCC; no execution, no module dependency, worktree-confined.
//
// Go-only by design: the precise win comes from the module's import paths. A
// non-Go / no-go.mod tree gets an honest "not a Go module" rather than a weak
// heuristic.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ImportGraphTool answers cycle / layering / deps / rdeps queries over a Go
// module's package import graph. Read-only.
type ImportGraphTool struct{}

func (ImportGraphTool) Name() string { return "import_graph" }
func (ImportGraphTool) Description() string {
	return "Read-only Go package dependency analysis: op=cycles (import cycles), layers (report imports " +
		"that violate an ordered high→low layer list), deps/rdeps (transitive imports / importers of a " +
		"package). Catches architectural drift `go build` tolerates. Go modules only. No writes, no execution."
}
func (ImportGraphTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"op":{"type":"string","enum":["cycles","layers","deps","rdeps"]},"pkg":{"type":"string"},"layers":{"type":"array","items":{"type":"string"}}},"required":[]}`)
}

func (ImportGraphTool) Run(ctx context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Op     string   `json:"op"`
		Pkg    string   `json:"pkg"`
		Layers []string `json:"layers"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("bad input: %w", err)
		}
	}
	if in.Op == "" {
		in.Op = "cycles"
	}

	module, err := moduledPath(workdir)
	if err != nil {
		return "", err
	}

	adj, pkgs, err := buildImportGraph(ctx, workdir, module)
	if err != nil {
		return "", err
	}
	if len(pkgs) == 0 {
		return "import_graph: no local packages found under the worktree", nil
	}

	switch in.Op {
	case "cycles":
		return renderCycles(adj, pkgs), nil
	case "layers":
		return renderLayers(adj, in.Layers), nil
	case "deps", "rdeps":
		return renderDeps(in.Op, adj, pkgs, normalizePkg(module, in.Pkg))
	default:
		return "", fmt.Errorf("unsupported op %q (want cycles|layers|deps|rdeps)", in.Op)
	}
}

// moduledPath reads the module path from the worktree's go.mod. A missing go.mod is
// a clear, non-fatal error (the tool only makes sense for a Go module).
func moduledPath(workdir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(workdir, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("import_graph: no go.mod in the worktree (Go modules only)")
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("import_graph: go.mod has no module directive")
}

// buildImportGraph parses every .go file's imports (ImportsOnly — cheap, no bodies)
// and folds them into a package-level adjacency map keyed by import path, keeping
// only edges to OTHER packages within this module. Returns the adjacency (pkg → set
// of imported local pkgs) and the sorted node list.
func buildImportGraph(ctx context.Context, workdir, module string) (map[string]map[string]bool, []string, error) {
	adj := map[string]map[string]bool{}
	nodes := map[string]bool{}
	fset := token.NewFileSet()

	err := filepath.WalkDir(workdir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		// Skip test files: an external test package (package foo_test in dir foo/)
		// shares the production package's directory, so folding its imports into the
		// dir-keyed node would manufacture FALSE cycles/edges. The layering + cycle
		// invariants are about the production import graph.
		if strings.HasSuffix(filepath.Base(path), "_test.go") {
			return nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil // unparseable file: skip, never fatal
		}
		pkg := pkgPath(module, workdir, path)
		nodes[pkg] = true
		for _, imp := range f.Imports {
			impPath, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if impPath == module || strings.HasPrefix(impPath, module+"/") {
				if impPath == pkg {
					continue // self-import within a package dir: not an edge
				}
				nodes[impPath] = true
				if adj[pkg] == nil {
					adj[pkg] = map[string]bool{}
				}
				adj[pkg][impPath] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	out := make([]string, 0, len(nodes))
	for n := range nodes {
		out = append(out, n)
	}
	sort.Strings(out)
	return adj, out, nil
}

// pkgPath maps a .go file to its package import path: module + "/" + (dir of file
// relative to the worktree root), with the root dir mapping to the bare module path.
func pkgPath(module, workdir, file string) string {
	rel, err := filepath.Rel(workdir, filepath.Dir(file))
	if err != nil || rel == "." || rel == "" {
		return module
	}
	return module + "/" + filepath.ToSlash(rel)
}

// normalizePkg accepts either a full import path or a worktree-relative dir and
// returns the canonical import path.
func normalizePkg(module, pkg string) string {
	pkg = strings.TrimSpace(pkg)
	if pkg == "" || pkg == module {
		return module
	}
	if strings.HasPrefix(pkg, module) {
		return pkg
	}
	return module + "/" + filepath.ToSlash(strings.TrimPrefix(pkg, "./"))
}

// renderCycles runs Tarjan's SCC algorithm and reports every strongly-connected
// component with more than one package — i.e. an import cycle.
func renderCycles(adj map[string]map[string]bool, pkgs []string) string {
	sccs := tarjanSCC(adj, pkgs)
	var cycles [][]string
	for _, c := range sccs {
		if len(c) > 1 {
			sort.Strings(c)
			cycles = append(cycles, c)
		}
	}
	sort.Slice(cycles, func(i, j int) bool { return cycles[i][0] < cycles[j][0] })
	var sb strings.Builder
	fmt.Fprintf(&sb, "import_graph cycles: %d cycle(s) over %d package(s).\n", len(cycles), len(pkgs))
	if len(cycles) == 0 {
		sb.WriteString("(acyclic)")
		return sb.String()
	}
	for i, c := range cycles {
		fmt.Fprintf(&sb, "cycle %d (%d pkgs): %s\n", i+1, len(c), strings.Join(c, " ⇄ "))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// renderLayers reports import edges that violate an ordered layer list. The list is
// HIGH-level → LOW-level; a higher layer may import a lower one, never the reverse.
// A package is assigned the layer of its longest matching prefix; packages matching
// no prefix are unlayered and skipped.
func renderLayers(adj map[string]map[string]bool, layers []string) string {
	if len(layers) == 0 {
		return "import_graph layers: provide an ordered high→low 'layers' prefix list to check (e.g. [\"mod/cmd\",\"mod/internal/agent\",\"mod/internal/tools\"])."
	}
	// Match on a path-SEGMENT boundary so a prefix like "mod/internal/ap" cannot claim
	// "mod/internal/apple/...": a package belongs to a layer only when it equals the
	// prefix or sits under it (prefix + "/").
	layerOf := func(pkg string) int {
		best, bestLen := -1, -1
		for i, pre := range layers {
			if (pkg == pre || strings.HasPrefix(pkg, pre+"/")) && len(pre) > bestLen {
				best, bestLen = i, len(pre)
			}
		}
		return best
	}
	type viol struct{ from, to string }
	var viols []viol
	for from, tos := range adj {
		lf := layerOf(from)
		if lf < 0 {
			continue
		}
		for to := range tos {
			lt := layerOf(to)
			if lt < 0 {
				continue
			}
			if lf > lt { // a lower layer importing a higher layer: wrong direction
				viols = append(viols, viol{from, to})
			}
		}
	}
	sort.Slice(viols, func(i, j int) bool {
		if viols[i].from != viols[j].from {
			return viols[i].from < viols[j].from
		}
		return viols[i].to < viols[j].to
	})
	var sb strings.Builder
	fmt.Fprintf(&sb, "import_graph layers: %d violation(s) (a lower layer importing a higher one).\n", len(viols))
	if len(viols) == 0 {
		sb.WriteString("(no layering violations)")
		return sb.String()
	}
	for _, v := range viols {
		fmt.Fprintf(&sb, "- %s → %s\n", v.from, v.to)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// renderDeps reports the transitive forward imports (deps) or reverse importers
// (rdeps) of a package.
func renderDeps(op string, adj map[string]map[string]bool, pkgs []string, pkg string) (string, error) {
	known := false
	for _, p := range pkgs {
		if p == pkg {
			known = true
			break
		}
	}
	if !known {
		return fmt.Sprintf("import_graph %s: package %q not found in this module", op, pkg), nil
	}
	// For rdeps, transpose the adjacency.
	g := adj
	if op == "rdeps" {
		t := map[string]map[string]bool{}
		for from, tos := range adj {
			for to := range tos {
				if t[to] == nil {
					t[to] = map[string]bool{}
				}
				t[to][from] = true
			}
		}
		g = t
	}
	seen := map[string]bool{pkg: true}
	queue := []string{pkg}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for next := range g[cur] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		if p != pkg {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	label := "transitive imports of"
	if op == "rdeps" {
		label = "transitive importers of"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "import_graph %s: %d %s %s\n", op, len(out), label, pkg)
	for _, p := range out {
		fmt.Fprintf(&sb, "- %s\n", p)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// tarjanSCC returns the strongly-connected components of the package graph using
// Tarjan's algorithm (iterative-free recursion is fine at package scale). Each
// component is a slice of package import paths.
func tarjanSCC(adj map[string]map[string]bool, pkgs []string) [][]string {
	index := 0
	idx := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var out [][]string

	// Deterministic neighbour order for reproducible components.
	neighbours := func(v string) []string {
		ns := make([]string, 0, len(adj[v]))
		for n := range adj[v] {
			ns = append(ns, n)
		}
		sort.Strings(ns)
		return ns
	}

	var strongconnect func(v string)
	strongconnect = func(v string) {
		idx[v] = index
		low[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range neighbours(v) {
			if _, ok := idx[w]; !ok {
				strongconnect(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] {
				if idx[w] < low[v] {
					low[v] = idx[w]
				}
			}
		}
		if low[v] == idx[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			out = append(out, comp)
		}
	}

	for _, v := range pkgs {
		if _, ok := idx[v]; !ok {
			strongconnect(v)
		}
	}
	return out
}
