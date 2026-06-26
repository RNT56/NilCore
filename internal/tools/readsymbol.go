package tools

// readsymbol.go — fetch exactly one declaration's source by name. The companion to
// the codeintel bundle and outline: after a graph hit gives the model a name and a
// filename, read_symbol slices out just that 30-line body instead of forcing a read
// of the whole 800-line file. Surgical reads are the token discipline the harness
// is built around. Read-only, worktree-confined, stdlib AST only; line-based spans
// (ast.Span carries StartLine/EndLine — there is no byte range).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"nilcore/internal/codeintel/ast"
)

// ReadSymbolTool returns the source of a single named declaration. On ambiguity it
// returns the candidate list and reads nothing — it never guesses.
type ReadSymbolTool struct{}

func (ReadSymbolTool) Name() string { return "read_symbol" }
func (ReadSymbolTool) Description() string {
	return "Read the source of ONE declaration (func/type/method/var/const) by name, instead of the whole " +
		"file. Optionally scope with 'file' and/or 'kind'. On ambiguity it lists the candidates and reads " +
		"nothing. No writes, no execution, no network."
}
func (ReadSymbolTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"file":{"type":"string"},"kind":{"type":"string"}},"required":["name"]}`)
}

func (ReadSymbolTool) Run(ctx context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Name string `json:"name"`
		File string `json:"file"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	if strings.TrimSpace(in.Name) == "" {
		return "", fmt.Errorf("name is required")
	}

	var matches []symRef

	if in.File != "" {
		// Scoped to one file: parse it directly.
		abs, err := safePath(workdir, in.File)
		if err != nil {
			return "", err
		}
		syms, serr := ast.Symbols(abs)
		if serr != nil {
			return "", fmt.Errorf("read_symbol %s: %w", in.File, serr)
		}
		rel := relOrSame(workdir, abs)
		for _, s := range syms {
			if symbolMatches(s, in.Name, in.Kind) {
				matches = append(matches, symRef{Name: s.Name, Kind: s.Kind, Recv: s.Recv, Rel: rel, Start: s.Span.StartLine, End: s.Span.EndLine})
			}
		}
	} else {
		// Resolve the bare name across the worktree.
		all, _, err := worktreeSymbols(ctx, workdir)
		if err != nil {
			return "", err
		}
		for _, s := range all {
			if s.Name == in.Name && (in.Kind == "" || string(s.Kind) == in.Kind) {
				matches = append(matches, s)
			}
		}
	}

	switch len(matches) {
	case 0:
		return fmt.Sprintf("read_symbol: %q not found (try outline or codeintel to locate it)", in.Name), nil
	case 1:
		return sliceSymbol(workdir, matches[0])
	default:
		return renderCandidates(in.Name, matches), nil
	}
}

// symbolMatches reports whether a symbol matches the requested name and optional
// kind (kind empty ⇒ any kind).
func symbolMatches(s ast.Symbol, name, kind string) bool {
	return s.Name == name && (kind == "" || string(s.Kind) == kind)
}

// sliceSymbol reads the matched declaration's line span out of its file and frames
// it with a `file:Lstart-Lend` header. The body itself is returned raw — the native
// loop fences every tool result as untrusted data before it reaches the model (I7).
func sliceSymbol(workdir string, m symRef) (string, error) {
	abs, err := safePath(workdir, m.Rel)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(b), "\n")
	start, end := m.Start, m.End
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start > end {
		return "", fmt.Errorf("read_symbol: %s has an empty span in %s", m.Name, m.Rel)
	}
	recv := ""
	if m.Recv != "" {
		recv = " (" + m.Recv + ")"
	}
	header := fmt.Sprintf("%s:L%d-%d  %s %s%s\n", m.Rel, start, end, m.Kind, m.Name, recv)
	return header + strings.Join(lines[start-1:end], "\n"), nil
}

// renderCandidates lists every match (sorted) so the model can disambiguate with a
// follow-up call carrying file/kind — no guessing across collisions.
func renderCandidates(name string, ms []symRef) string {
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].Rel != ms[j].Rel {
			return ms[i].Rel < ms[j].Rel
		}
		return ms[i].Start < ms[j].Start
	})
	var sb strings.Builder
	fmt.Fprintf(&sb, "read_symbol: %q is ambiguous (%d matches) — re-call with 'file' and/or 'kind':\n", name, len(ms))
	for _, m := range ms {
		recv := ""
		if m.Recv != "" {
			recv = " recv=" + m.Recv
		}
		fmt.Fprintf(&sb, "- %s:L%d  %s%s\n", m.Rel, m.Start, m.Kind, recv)
	}
	return strings.TrimRight(sb.String(), "\n")
}
