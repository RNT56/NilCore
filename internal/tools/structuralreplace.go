package tools

// structuralreplace.go — a formatting-insensitive, AST-structural search-and-rewrite
// over Go expressions (the gofmt -r idea, made a worktree-confined tool). It is
// "stronger than grep": `errors.New(fmt.Sprintf(msg))` matches regardless of
// whitespace, line breaks, or comments, and never matches inside a string/comment.
// Metavariables are named explicitly in `vars` so the pattern stays unambiguous Go.
//
// SAFETY: matching is structural; substitution is text (the user's rewrite string
// with each var replaced by the matched source), and the whole rewritten file is
// re-run through go/format.Source — if it does not parse, the file is REJECTED and
// nothing is written. dry_run defaults TRUE and a match cap bounds the sweep. The
// match is SYNTACTIC, not type-aware (no import resolution) — better than grep, not
// refactoring-IDE-grade. Go only; other languages are skipped.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// srFileResult is one file's matches plus, when rewriting, the formatted bytes to
// write (nil if the rewrite did not parse — rejected).
type srFileResult struct {
	rel      string
	matches  []string
	newBytes []byte
	rejected bool
}

// StructuralReplaceTool finds (and optionally rewrites) Go expression patterns.
type StructuralReplaceTool struct{}

func (StructuralReplaceTool) Name() string { return "structural_replace" }
func (StructuralReplaceTool) Description() string {
	return "Structural (AST) find/replace over Go expressions — formatting-insensitive, ignores strings/" +
		"comments. 'pattern' and 'rewrite' are Go expressions; 'vars' names the wildcard identifiers that " +
		"match any sub-expression. dry_run defaults true (preview). Re-formats and re-parses before writing " +
		"(rejects a result that does not compile-parse). Go only. Stronger than grep, not type-aware."
}
func (StructuralReplaceTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"rewrite":{"type":"string"},"vars":{"type":"array","items":{"type":"string"}},"glob":{"type":"string"},"dry_run":{"type":"boolean"},"max_matches":{"type":"integer"}},"required":["pattern"]}`)
}

const defaultStructuralCap = 50

func (StructuralReplaceTool) Run(ctx context.Context, workdir string, input json.RawMessage) (string, error) {
	var in struct {
		Pattern    string   `json:"pattern"`
		Rewrite    string   `json:"rewrite"`
		Vars       []string `json:"vars"`
		Glob       string   `json:"glob"`
		DryRun     *bool    `json:"dry_run"`
		MaxMatches int      `json:"max_matches"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	if strings.TrimSpace(in.Pattern) == "" {
		return "", fmt.Errorf("pattern is required")
	}
	dryRun := true
	if in.DryRun != nil {
		dryRun = *in.DryRun
	}
	limit := in.MaxMatches
	if limit <= 0 {
		limit = defaultStructuralCap
	}
	vars := map[string]bool{}
	for _, v := range in.Vars {
		if v != "" {
			vars[v] = true
		}
	}

	patExpr, err := parser.ParseExpr(in.Pattern)
	if err != nil {
		return "", fmt.Errorf("pattern is not a Go expression: %w", err)
	}
	if id, ok := patExpr.(*ast.Ident); ok && vars[id.Name] {
		return "", fmt.Errorf("pattern cannot be a bare wildcard (it would match every expression)")
	}
	rewriting := strings.TrimSpace(in.Rewrite) != ""
	if rewriting {
		if _, perr := parser.ParseExpr(in.Rewrite); perr != nil {
			return "", fmt.Errorf("rewrite is not a Go expression: %w", perr)
		}
	}

	files, err := sourceFilesUnder(workdir)
	if err != nil {
		return "", err
	}

	var results []srFileResult
	totalMatches := 0

	for _, path := range files {
		if filepath.Ext(path) != ".go" {
			continue
		}
		if in.Glob != "" {
			if ok, _ := filepath.Match(in.Glob, filepath.Base(path)); !ok {
				continue
			}
		}
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		if totalMatches >= limit {
			break
		}

		fset := token.NewFileSet()
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		f, perr := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if perr != nil {
			continue
		}
		eng := &srEngine{vars: vars, src: src, fset: fset}
		type hit struct {
			start, end int
			line       int
			binds      map[string]string
			text       string
		}
		var hits []hit
		ast.Inspect(f, func(node ast.Node) bool {
			expr, ok := node.(ast.Expr)
			if !ok {
				return true
			}
			binds := map[string]string{}
			if eng.match(reflect.ValueOf(patExpr), reflect.ValueOf(expr), binds) {
				s := fset.Position(expr.Pos())
				e := fset.Position(expr.End())
				hits = append(hits, hit{start: s.Offset, end: e.Offset, line: s.Line, binds: binds, text: string(src[s.Offset:e.Offset])})
				return false // do not descend into a matched subtree
			}
			return true
		})
		if len(hits) == 0 {
			continue
		}

		res := srFileResult{rel: relOrSame(workdir, path)}
		// Collect the ACCEPTED hits (document order) under the shared cap, so the
		// rewrite applies EXACTLY the matches the report shows — never more.
		var accepted []hit
		for _, h := range hits {
			if totalMatches >= limit {
				break
			}
			res.matches = append(res.matches, fmt.Sprintf("L%d: %s", h.line, oneLineSnippet(h.text)))
			accepted = append(accepted, h)
			totalMatches++
		}

		if rewriting && len(accepted) > 0 {
			// Apply text splices right-to-left so earlier offsets stay valid. Only the
			// accepted (capped) hits are written — the written diff equals the report.
			sort.Slice(accepted, func(i, j int) bool { return accepted[i].start > accepted[j].start })
			out := append([]byte(nil), src...)
			for _, h := range accepted {
				repl := substituteRewrite(in.Rewrite, h.binds)
				out = append(out[:h.start], append([]byte(repl), out[h.end:]...)...)
			}
			if formatted, ferr := format.Source(out); ferr == nil {
				res.newBytes = formatted
			} else {
				res.rejected = true
			}
		}
		results = append(results, res)
	}

	return renderStructural(in.Pattern, in.Rewrite, rewriting, dryRun, totalMatches, limit, results, workdir)
}

// renderStructural builds the report and, when not a dry run, writes the accepted
// rewrites. Kept separate so the Run body stays readable.
func renderStructural(pattern, rewrite string, rewriting, dryRun bool, total, limit int, results []srFileResult, workdir string) (string, error) {
	_ = rewrite
	var sb strings.Builder
	head := "structural_replace (find)"
	if rewriting {
		if dryRun {
			head = "structural_replace (rewrite, DRY RUN)"
		} else {
			head = "structural_replace (rewrite, APPLIED)"
		}
	}
	fmt.Fprintf(&sb, "%s: pattern %q → %d match(es) in %d file(s)", head, pattern, total, len(results))
	if total >= limit {
		fmt.Fprintf(&sb, " (capped at %d — narrow with glob/max_matches)", limit)
	}
	sb.WriteByte('\n')
	if rewriting {
		sb.WriteString("MATCHING IS SYNTACTIC (not type-aware); review the diff. The verifier still governs done.\n")
	}

	wrote := 0
	for _, r := range results {
		fmt.Fprintf(&sb, "\n%s (%d match)\n", r.rel, len(r.matches))
		for _, m := range r.matches {
			fmt.Fprintf(&sb, "  %s\n", m)
		}
		if rewriting {
			if r.rejected {
				sb.WriteString("  ! rewrite would not parse — file SKIPPED (no write)\n")
				continue
			}
			if r.newBytes == nil {
				continue
			}
			if !dryRun {
				p, perr := safePath(workdir, r.rel)
				if perr != nil {
					return "", perr
				}
				if werr := writeNoFollow(workdir, p, r.newBytes); werr != nil {
					return "", werr
				}
				wrote++
				sb.WriteString("  ✓ rewritten\n")
			} else {
				sb.WriteString("  (would rewrite; pass dry_run=false to apply)\n")
			}
		}
	}
	if rewriting && !dryRun {
		fmt.Fprintf(&sb, "\nwrote %d file(s).", wrote)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// substituteRewrite renders the rewrite text with each var replaced by its bound
// source (word-boundary, so a var name is never matched inside a larger token).
func substituteRewrite(rewrite string, binds map[string]string) string {
	if len(binds) == 0 {
		return rewrite
	}
	// Build ONE alternation of all metavar names and substitute in a SINGLE pass, so
	// a bound value that happens to contain another var's name is never re-scanned
	// (which would corrupt the output, order-dependently). Longer names first so a
	// var that is a prefix of another cannot shadow it.
	names := make([]string, 0, len(binds))
	for n := range binds {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	re := regexp.MustCompile(`\b(?:` + strings.Join(quoted, "|") + `)\b`)
	return re.ReplaceAllStringFunc(rewrite, func(m string) string {
		if v, ok := binds[m]; ok {
			return v
		}
		return m
	})
}

// oneLineSnippet collapses a multi-line match to a single line for the report.
func oneLineSnippet(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:117] + "…"
	}
	return s
}

// srEngine holds the per-file matching context.
type srEngine struct {
	vars map[string]bool
	src  []byte
	fset *token.FileSet
}

var (
	identPtrType = reflect.TypeOf((*ast.Ident)(nil))
	posType      = reflect.TypeOf(token.NoPos)
)

// skipField names the ast struct fields that must not participate in structural
// comparison: resolver back-pointers (Obj/Scope/Unresolved) and attached comments
// (Doc/Comment), which are not part of expression shape. Skipping by NAME avoids
// referencing the deprecated ast.Object / ast.Scope types.
var skipField = map[string]bool{"Obj": true, "Scope": true, "Unresolved": true, "Doc": true, "Comment": true}

// text returns the source slice spanning node n.
func (e *srEngine) text(n ast.Node) string {
	s := e.fset.Position(n.Pos()).Offset
	t := e.fset.Position(n.End()).Offset
	if s < 0 || t > len(e.src) || s > t {
		return ""
	}
	return string(e.src[s:t])
}

// match compares a pattern reflect.Value against a value reflect.Value structurally,
// ignoring positions/objects/scopes/comments. A wildcard ident in the pattern binds
// to any sub-expression (and a repeated wildcard must bind to identical source).
func (e *srEngine) match(pattern, val reflect.Value, binds map[string]string) bool {
	if name, ok := wildcardName(pattern); ok && e.vars[name] {
		node, ok := valNode(val)
		if !ok {
			return false
		}
		txt := e.text(node)
		if prev, seen := binds[name]; seen {
			return prev == txt
		}
		binds[name] = txt
		return true
	}
	if !pattern.IsValid() || !val.IsValid() {
		return pattern.IsValid() == val.IsValid()
	}
	if pattern.Type() != val.Type() {
		return false
	}
	switch pattern.Kind() {
	case reflect.Interface, reflect.Pointer:
		if pattern.IsNil() || val.IsNil() {
			return pattern.IsNil() == val.IsNil()
		}
		return e.match(pattern.Elem(), val.Elem(), binds)
	case reflect.Slice:
		if pattern.Len() != val.Len() {
			return false
		}
		for i := 0; i < pattern.Len(); i++ {
			if !e.match(pattern.Index(i), val.Index(i), binds) {
				return false
			}
		}
		return true
	case reflect.Struct:
		for i := 0; i < pattern.NumField(); i++ {
			sf := pattern.Type().Field(i)
			if sf.Type == posType || skipField[sf.Name] {
				continue
			}
			if !e.match(pattern.Field(i), val.Field(i), binds) {
				return false
			}
		}
		return true
	case reflect.String:
		return pattern.String() == val.String()
	default:
		return pattern.Interface() == val.Interface()
	}
}

// wildcardName returns the identifier name if v is (an interface holding) a
// *ast.Ident, so the caller can test it against the wildcard set.
func wildcardName(v reflect.Value) (string, bool) {
	if !v.IsValid() {
		return "", false
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return "", false
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.Pointer && v.Type() == identPtrType && !v.IsNil() {
		return v.Interface().(*ast.Ident).Name, true
	}
	return "", false
}

// valNode resolves a reflect.Value (possibly an interface) to the ast.Node it holds.
func valNode(v reflect.Value) (ast.Node, bool) {
	if !v.IsValid() {
		return nil, false
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil, false
		}
		v = v.Elem()
	}
	if !v.CanInterface() {
		return nil, false
	}
	n, ok := v.Interface().(ast.Node)
	return n, ok
}
