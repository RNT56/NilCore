// Pure-Go JavaScript/TypeScript backend (R2). Like the Python backend, this is a
// deliberately lightweight, brace-aware line scanner over the standard library
// (bufio/regexp/strings) — NOT a full grammar. The goal is to reliably surface the
// structure a code-intel index needs for *typical* JS/TS — top-level functions,
// classes and their methods, arrow/function variable bindings, the line spans of
// those bodies, and the names they call — not to validate or fully model the
// language. TypeScript is handled as a JS superset: type annotations, generics, and
// the `interface`/`type` syntax we don't model are simply ignored, which is safe for
// the structural facts we extract. Pathological inputs (deeply nested ternaries that
// look like arrows, regex literals containing braces, JSX with embedded expressions)
// may be approximated; that is an accepted trade for staying stdlib-only and cgo-free.
//
// Span model: JS bodies are brace-delimited, so a header spans from its line to the
// line of the matching close brace. We track net brace depth over comment/string-
// stripped text (see brace.go) and close a header when depth returns to its open
// level. Headers whose body opens on a later line (a `{` not on the header line) are
// still captured as single-line symbols — an accepted approximation.
package ast

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"
)

// Header patterns. Each captures the declared name. We tolerate `export` /
// `export default` prefixes by allowing them (non-capturing) before the keyword, so
// the name capture is never shifted by an export modifier.
var (
	// `function NAME(` and `async function NAME(` — a free function declaration.
	jsFuncRe = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)
	// `class NAME` (optionally `extends ...`). Name capture stops at whitespace.
	jsClassRe = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][A-Za-z0-9_$]*)`)
	// `const NAME = (...) =>` / `const NAME = async (...) =>` / `const NAME = x =>` —
	// an arrow-function binding. let/var are accepted too. A TS return-type annotation
	// (`): T =>`) is tolerated between the param list and the arrow; the `[^=>]` class
	// keeps it from swallowing the `=>` itself.
	jsArrowRe = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*(?::[^=]+)?=\s*(?:async\s+)?(?:\([^)]*\)\s*(?::[^=>]+)?|[A-Za-z_$][A-Za-z0-9_$]*)\s*=>`)
	// `const NAME = function` — a function-expression binding.
	jsFuncExprRe = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*(?::[^=]+)?=\s*(?:async\s+)?function`)
	// A class-body method: `NAME(...) {` or `async NAME(...) {` or `get NAME()` etc.
	// Only meaningful while we are inside a class body (depth/Recv tracked by the
	// scanner). The leading anchor allows indentation and common modifiers.
	jsMethodRe = regexp.MustCompile(`^\s*(?:public\s+|private\s+|protected\s+|static\s+|async\s+|get\s+|set\s+|\*\s*)*([A-Za-z_$][A-Za-z0-9_$]*)\s*\([^;{]*\)\s*(?::\s*[^={]+)?\{`)
	// Call sites: an identifier (optionally dotted, e.g. obj.method) immediately
	// followed by "(". We keep the trailing simple name as the callee, mirroring the
	// Go/Python backends' selector handling.
	jsCallRe = regexp.MustCompile(`([A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)*)\s*\(`)
	// Keywords that take a "(" but are control flow / operators, not calls.
	jsKeyword = map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "catch": true,
		"function": true, "return": true, "typeof": true, "await": true,
		"new": true, "delete": true, "void": true, "throw": true, "in": true,
		"of": true, "instanceof": true, "yield": true, "case": true, "do": true,
		"else": true, "super": true, "import": true, "export": true,
	}
)

// jsParser scans JS/TS source line-by-line. It is stateless across calls.
type jsParser struct{}

func (jsParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := jsScan(path, false)
	return syms, err
}

func (jsParser) references(path string) ([]Reference, error) {
	_, refs, err := jsScan(path, true)
	return refs, err
}

func (jsParser) calls(path string) (map[string][]string, error) {
	// Reuse the single pass: it attributes each call to its enclosing function via the
	// symbol spans, which is exactly the call-graph grouping. groupBraceCalls is shared
	// with the Rust backend so the two agree on how calls map to owners.
	syms, refs, err := jsScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// jsScan is the shared single pass. It always builds symbols; when wantRefs is set it
// also collects call references. Returning both keeps the three public methods
// consistent (they agree on spans because they share this walk).
func jsScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := openSource(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open js file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	var syms []Symbol
	var refs []Reference
	var stack []braceBlock // open brace blocks, outermost first
	var st stripState      // cross-line string/comment carry
	depth := 0             // net brace depth over stripped text

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		// Strip strings/comments BEFORE any structural decision so braces, quotes, and
		// decoy identifiers inside literals never affect headers, spans, or calls.
		code, nextSt := stripLine(raw, st)
		depthBefore := depth
		delta := braceDelta(code)

		// Extend the span of every still-open ancestor to include this line, then close
		// any whose matching brace we just passed (depth fell back to their open level).
		extendBraceSpans(syms, stack, lineNo)

		// Header detection. We test the most specific shapes first. A class opens a
		// receiver block; a free function / arrow / function-expression opens a plain
		// block; a bare method only counts when the innermost open block is a class.
		switch {
		case matchHeader(jsClassRe, code) != "":
			name := matchHeader(jsClassRe, code)
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore, isRecv: true})

		case matchHeader(jsFuncRe, code) != "":
			pushBraceFunc(&syms, &stack, path, lineNo, matchHeader(jsFuncRe, code), nearestRecv(syms, stack), KindFunc, depthBefore)
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, jsScanCalls)

		case matchHeader(jsArrowRe, code) != "" || matchHeader(jsFuncExprRe, code) != "":
			name := matchHeader(jsArrowRe, code)
			if name == "" {
				name = matchHeader(jsFuncExprRe, code)
			}
			pushBraceFunc(&syms, &stack, path, lineNo, name, "", KindFunc, depthBefore)
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, jsScanCalls)

		case insideClass(stack) && matchHeader(jsMethodRe, code) != "" && !jsKeyword[matchHeader(jsMethodRe, code)]:
			name := matchHeader(jsMethodRe, code)
			pushBraceFunc(&syms, &stack, path, lineNo, name, nearestRecv(syms, stack), KindMethod, depthBefore)
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, jsScanCalls)

		default:
			if wantRefs {
				refs = append(refs, jsScanCalls(code, path, lineNo)...)
			}
		}

		depth += delta
		closeBraceBlocks(&stack, depth)
		st = nextSt
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan js file: %w", err)
	}
	return syms, refs, nil
}

// jsScanCalls extracts call references from one stripped line. It drops matches whose
// trailing simple name is a JS keyword and keeps the trailing name for dotted calls
// (obj.method() -> "method"), mirroring the Go/Python backends.
func jsScanCalls(code, path string, lineNo int) []Reference {
	var out []Reference
	for _, m := range jsCallRe.FindAllStringSubmatch(code, -1) {
		name := trailingName(m[1])
		if jsKeyword[name] {
			continue
		}
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}

// insideClass reports whether the innermost open block on the stack is a receiver
// (class) block — the condition for treating a bare `NAME() {` as a method.
func insideClass(stack []braceBlock) bool {
	if len(stack) == 0 {
		return false
	}
	return stack[len(stack)-1].isRecv
}

// scanHeaderCalls records default-value / argument calls that appear on a header line
// AFTER the parameter-list opener, so the function's own name is never recorded as a
// self-call (the same fix the Python backend applies). It is a no-op when wantRefs is
// false.
func scanHeaderCalls(refs *[]Reference, code, path string, lineNo int, wantRefs bool, scan func(string, string, int) []Reference) {
	if !wantRefs {
		return
	}
	if op := strings.IndexByte(code, '('); op >= 0 {
		*refs = append(*refs, scan(code[op+1:], path, lineNo)...)
	}
}
