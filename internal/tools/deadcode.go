package tools

// deadcode.go — a read-only "what is never reached?" lens. It builds the same
// ephemeral call graph the codeintel tool uses, seeds a root set (exported API +
// Test/Benchmark/Example/Fuzz + main/init), and reports the functions/methods NOT
// in the forward closure of those roots. It reuses impact's reverse-reachability
// machinery in the forward direction (forwardReachable).
//
// HONEST LIMITS (surfaced in the output header, not hidden): the graph is the
// FUNCTION call graph keyed by bare symbol name, so (a) it only reasons about
// funcs/methods — types/vars/consts are out of scope; (b) a function passed as a
// value, invoked via an interface, or reached by reflection can be a FALSE
// POSITIVE; (c) same-named functions in different packages collapse. Output is
// therefore an advisory list of CANDIDATES to confirm (LSP/human) before deleting,
// never an authority — exactly the posture the verifier-owns-truth thesis wants.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"nilcore/internal/codeintel/ast"
)

// DeadCodeTool reports unreachable functions/methods. Read-only, no write surface.
type DeadCodeTool struct{}

func (DeadCodeTool) Name() string { return "dead_code" }
func (DeadCodeTool) Description() string {
	return "Read-only: list functions/methods never reached from the exported API, tests, or main/init " +
		"via the call graph — candidate dead code. ADVISORY only (a func used via an interface, as a value, " +
		"or by reflection may be a false positive); confirm before deleting. No writes, no execution."
}
func (DeadCodeTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"roots":{"type":"array","items":{"type":"string"}},"include_tests":{"type":"boolean"}},"required":[]}`)
}

func (DeadCodeTool) Run(ctx context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Roots        []string `json:"roots"`
		IncludeTests bool     `json:"include_tests"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("bad input: %w", err)
		}
	}

	syms, indexed, err := worktreeSymbols(ctx, workdir)
	if err != nil {
		return "", err
	}
	g, _, err := buildGraph(ctx, workdir)
	if err != nil {
		return "", err
	}
	defer g.Close()

	// Seed the root set: caller-supplied roots plus the auto roots (exported
	// funcs/methods, test/benchmark/example/fuzz entry points, main, init).
	rootSet := map[string]bool{}
	for _, r := range in.Roots {
		if r != "" {
			rootSet[r] = true
		}
	}
	for _, s := range syms {
		if s.Kind != ast.KindFunc && s.Kind != ast.KindMethod {
			continue
		}
		if isEntryPointFunc(s.Name) || isExportedName(s.Name) {
			rootSet[s.Name] = true
		}
	}
	roots := make([]string, 0, len(rootSet))
	for r := range rootSet {
		roots = append(roots, r)
	}

	reachable, err := forwardReachable(ctx, g, roots)
	if err != nil {
		return "", err
	}

	// Candidates: funcs/methods not reachable from any root. De-dup by name+file so
	// a method set rendered once. Tests are excluded unless include_tests is set
	// (a Test* function is itself a root, but its unexported helpers would surface).
	type cand struct {
		name, kind, recv, rel string
		line                  int
	}
	seen := map[string]bool{}
	var cands []cand
	for _, s := range syms {
		if s.Kind != ast.KindFunc && s.Kind != ast.KindMethod {
			continue
		}
		if reachable[s.Name] {
			continue
		}
		if !in.IncludeTests && strings.HasSuffix(s.Rel, "_test.go") {
			continue
		}
		key := s.Rel + "\x00" + s.Recv + "\x00" + s.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		cands = append(cands, cand{name: s.Name, kind: string(s.Kind), recv: s.Recv, rel: s.Rel, line: s.Start})
	}

	sort.Slice(cands, func(i, j int) bool {
		if cands[i].rel != cands[j].rel {
			return cands[i].rel < cands[j].rel
		}
		return cands[i].line < cands[j].line
	})

	var sb strings.Builder
	fmt.Fprintf(&sb, "dead_code: %d candidate unreachable func/method(s) over %d indexed file(s).\n", len(cands), indexed)
	sb.WriteString("ADVISORY — call-graph + bare-name analysis; interface/callback/reflection uses can be false positives. Confirm before deleting.\n")
	if len(cands) == 0 {
		sb.WriteString("(none found)")
		return sb.String(), nil
	}
	for _, c := range cands {
		recv := ""
		if c.recv != "" {
			recv = " (" + c.recv + ")"
		}
		fmt.Fprintf(&sb, "- %s:%d  %s %s%s\n", c.rel, c.line, c.kind, c.name, recv)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// isEntryPointFunc reports whether a function name is a conventional entry point
// that is "used" even with no in-repo caller: the testing/fuzzing harness names and
// the program/package init names.
func isEntryPointFunc(name string) bool {
	if name == "main" || name == "init" {
		return true
	}
	for _, p := range []string{"Test", "Benchmark", "Example", "Fuzz"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
