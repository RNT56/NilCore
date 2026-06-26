package tools

// outline.go — a read-only, host-side structure view. "Show me the shape of this
// file/directory before I read or edit it." It turns ast.Symbols spans into a
// compact roster the model can scan instead of slurping whole files into context
// (north-star principle #3: context is the scarce resource). Deterministic,
// worktree-confined, stdlib-only; degrades by construction on mixed-language trees
// (a file the AST layer cannot parse contributes nothing, never an error).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"nilcore/internal/codeintel/ast"
)

// OutlineTool renders the declared symbols of a file, or a per-file skeleton of a
// directory subtree, as an ordered roster. Read-only: it parses but never writes.
type OutlineTool struct{}

func (OutlineTool) Name() string { return "outline" }
func (OutlineTool) Description() string {
	return "Read-only structure view: list the declared symbols (funcs/types/methods/vars/consts) of a " +
		"file, or a per-file skeleton of a directory, with their line ranges. Use it to see a file's shape " +
		"before reading the whole thing. No writes, no execution, no network."
}
func (OutlineTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"budget":{"type":"integer"}},"required":["path"]}`)
}

// defaultOutlineBudget bounds how many symbol lines a single outline emits, so a
// huge package can never flood the model's context. The walk is deterministic, so
// the cap selects a stable prefix.
const defaultOutlineBudget = 200

func (OutlineTool) Run(ctx context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Path   string `json:"path"`
		Budget int    `json:"budget"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	if strings.TrimSpace(in.Path) == "" {
		return "", fmt.Errorf("path is required")
	}
	budget := in.Budget
	if budget <= 0 {
		budget = defaultOutlineBudget
	}
	abs, err := safePath(workdir, in.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	emitted := 0

	if !info.IsDir() {
		syms, serr := ast.Symbols(abs)
		if serr != nil {
			return "", fmt.Errorf("outline %s: %w", in.Path, serr)
		}
		if len(syms) == 0 {
			return fmt.Sprintf("outline %s: no symbols (unsupported language or empty file)", in.Path), nil
		}
		fmt.Fprintf(&sb, "outline %s\n", in.Path)
		writeSymbolLines(&sb, syms, budget, &emitted)
		if emitted >= budget {
			sb.WriteString("… (truncated; raise budget for more)")
		}
		return strings.TrimRight(sb.String(), "\n"), nil
	}

	// Directory: a per-file skeleton over the supported source files beneath it.
	files, ferr := sourceFilesUnder(abs)
	if ferr != nil {
		return "", fmt.Errorf("outline %s: %w", in.Path, ferr)
	}
	fmt.Fprintf(&sb, "outline %s/ (%d source file(s))\n", strings.TrimRight(in.Path, "/"), len(files))
	for _, f := range files {
		if emitted >= budget {
			sb.WriteString("… (truncated; raise budget or outline a single file)")
			break
		}
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		syms, serr := ast.Symbols(f)
		if serr != nil || len(syms) == 0 {
			continue
		}
		rel, _ := filepath.Rel(abs, f)
		fmt.Fprintf(&sb, "\n%s\n", rel)
		writeSymbolLines(&sb, syms, budget, &emitted)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// writeSymbolLines renders symbols as "  kind name [recv] (Lstart-Lend)" in source
// order, stopping at the shared budget. Methods carry their receiver so the roster
// is unambiguous; exported names are starred so the model sees the public surface.
func writeSymbolLines(sb *strings.Builder, syms []ast.Symbol, budget int, emitted *int) {
	ss := make([]ast.Symbol, len(syms))
	copy(ss, syms)
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].Span.StartLine < ss[j].Span.StartLine })
	for _, s := range ss {
		if *emitted >= budget {
			return
		}
		name := s.Name
		if s.Recv != "" {
			name = "(" + s.Recv + ") " + s.Name
		}
		star := " "
		if isExportedName(s.Name) {
			star = "*"
		}
		fmt.Fprintf(sb, " %s%s %s (L%d-%d)\n", star, s.Kind, name, s.Span.StartLine, s.Span.EndLine)
		*emitted++
	}
}
