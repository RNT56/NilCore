// Pure-Go Python backend (D3-T01). Python has no standard-library parser we can
// reach from Go, and a cgo tree-sitter binding is forbidden by the zero-cgo
// invariant (I6) — so this is a deliberately lightweight, indentation-aware line
// scanner over the standard library (bufio/regexp/strings). It is NOT a full
// grammar: the goal is to reliably surface the structure a code-intel index needs
// for *typical* Python — top-level and nested `def`/`class`, methods (a `def`
// indented inside a `class`), their line spans, and the names they call — not to
// validate or fully model the language. Pathological inputs (heavy use of
// continuation lines inside a signature, semicolon-joined statements, exotic
// string-prefix combinations) may be approximated; that is an accepted trade for
// staying stdlib-only and cgo-free.
//
// Span model: Python blocks are delimited by indentation, so a def/class spans from
// its header line to the last line more-indented than the header (blank/comment-only
// lines don't end a block). We compute that by remembering each open block's header
// indent and closing it when a later code line dedents to or past it.
package ast

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"
)

// Header patterns. We anchor on the (possibly indented) keyword and capture the
// name. `async def` is accepted. The trailing context after the name is left to the
// caller; we only need the identifier here.
var (
	pyDefRe   = regexp.MustCompile(`^(\s*)(?:async\s+)?def\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	pyClassRe = regexp.MustCompile(`^(\s*)class\s+([A-Za-z_][A-Za-z0-9_]*)\s*[\(:]`)
	// Call sites: an identifier (optionally dotted, e.g. obj.method) immediately
	// followed by "(". We capture the trailing simple name as the callee, matching
	// the Go backend's behavior of recording the selected name for selector calls.
	pyCallRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\(`)
	// def/class keywords used as call-like text ("def f(") must not be mistaken for
	// calls; we strip headers before scanning a line for calls, but this guards the
	// keyword tokens directly too.
	pyKeyword = map[string]bool{
		"def": true, "class": true, "if": true, "elif": true, "while": true,
		"for": true, "with": true, "return": true, "yield": true, "assert": true,
		"del": true, "raise": true, "except": true, "lambda": true, "print": true,
		"and": true, "or": true, "not": true, "in": true, "is": true,
	}
)

// pythonParser scans Python source line-by-line. It is stateless across calls.
type pythonParser struct{}

// pyBlock tracks an open def/class while we walk the file, so we can both nest
// (a def inside a class is a method) and compute spans by indentation. We store the
// symbol's INDEX into the result slice rather than a pointer: the slice grows via
// append as we discover symbols, which can relocate the backing array and dangle any
// held pointer — an index stays valid across reallocation.
type pyBlock struct {
	idx        int  // index into the syms slice
	headIndent int  // indent width of the header line
	isClass    bool // class headers make nested defs methods
}

func (pythonParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := pythonScan(path, false)
	return syms, err
}

func (pythonParser) references(path string) ([]Reference, error) {
	_, refs, err := pythonScan(path, true)
	return refs, err
}

func (pythonParser) calls(path string) (map[string][]string, error) {
	// Reuse the single-pass scanner: it already attributes each call line to its
	// enclosing def/method, which is exactly the call-graph grouping. We rebuild the
	// map from references plus the symbol spans so there is one source of truth for
	// "which calls happen inside which function".
	syms, refs, err := pythonScan(path, true)
	if err != nil {
		return nil, err
	}
	return pythonGroupCalls(syms, refs), nil
}

// pythonScan is the shared single pass. It always builds symbols; when wantRefs is
// set it also collects call references (cheap to gate so the symbols-only path does
// no regex work for calls). Returning both keeps the three public methods consistent
// (they agree on spans because they share this walk).
func pythonScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := openSource(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open python file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	var syms []Symbol
	var refs []Reference
	var stack []pyBlock // open blocks, outermost first

	sc := bufio.NewScanner(f)
	// Allow long lines (generated code, data literals) without erroring.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		code := stripPyComment(raw)
		if strings.TrimSpace(code) == "" {
			// Blank or comment-only lines neither open, close, nor extend a block:
			// in Python they are not part of any suite for span purposes.
			continue
		}
		indent := leadingIndent(code)

		// Close any open blocks this line has dedented out of. A line at indent <=
		// a block's header indent ends that block; the block's span already covers
		// every more-indented line seen before now (we extend EndLine below).
		for len(stack) > 0 && indent <= stack[len(stack)-1].headIndent {
			stack = stack[:len(stack)-1]
		}

		// Extend the span of every still-open ancestor to include this line.
		for i := range stack {
			if lineNo > syms[stack[i].idx].Span.EndLine {
				syms[stack[i].idx].Span.EndLine = lineNo
			}
		}

		// Header detection: a line may open a class or a def. We map a def to
		// KindMethod when its nearest open block is a class, else KindFunc.
		if m := pyClassRe.FindStringSubmatch(code); m != nil {
			syms = append(syms, Symbol{
				Name: m[2],
				Kind: KindType,
				Span: Span{File: path, StartLine: lineNo, EndLine: lineNo},
			})
			stack = append(stack, pyBlock{idx: len(syms) - 1, headIndent: indent, isClass: true})
			continue
		}
		if m := pyDefRe.FindStringSubmatch(code); m != nil {
			kind := KindFunc
			recv := ""
			if cls := nearestClass(syms, stack); cls != "" {
				kind = KindMethod
				recv = cls
			}
			syms = append(syms, Symbol{
				Name: m[2],
				Kind: kind,
				Recv: recv,
				Span: Span{File: path, StartLine: lineNo, EndLine: lineNo},
			})
			stack = append(stack, pyBlock{idx: len(syms) - 1, headIndent: indent})
			// A def header can itself contain default-value calls, e.g.
			// `def f(x=g()):`. Scan ONLY the part after the parameter-list opener so
			// the function's own name is never recorded as a self-call, while
			// default-value calls are still captured.
			if wantRefs {
				if op := strings.IndexByte(code, '('); op >= 0 {
					refs = append(refs, scanPyCalls(code[op+1:], path, lineNo)...)
				}
			}
			continue
		}

		if wantRefs {
			refs = append(refs, scanPyCalls(code, path, lineNo)...)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan python file: %w", err)
	}
	return syms, refs, nil
}

// scanPyCalls extracts call references from one line of (comment-stripped) code.
// It deliberately tolerates false negatives over false positives: we drop matches
// whose name is a Python keyword (so `if (`, `def foo(` don't read as calls) and
// keep only the trailing simple name for dotted calls (obj.method() -> "method"),
// mirroring the Go backend's selector handling.
func scanPyCalls(code, path string, lineNo int) []Reference {
	// Strip string literals so quoted "(" or words don't masquerade as calls.
	code = blankPyStrings(code)
	var out []Reference
	for _, m := range pyCallRe.FindAllStringSubmatch(code, -1) {
		full := m[1]
		name := full
		if dot := strings.LastIndex(full, "."); dot >= 0 {
			name = full[dot+1:]
		}
		if pyKeyword[name] {
			continue
		}
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}

// pythonGroupCalls attributes each call reference to the innermost def/method whose
// span contains it, producing the per-function call map. Calls outside any function
// (module-level statements) are intentionally dropped — the call graph is about
// function-to-function edges, matching the Go backend, which only walks function
// bodies.
func pythonGroupCalls(syms []Symbol, refs []Reference) map[string][]string {
	out := map[string][]string{}
	// Ensure every function/method appears as a key even if it calls nothing, so the
	// caller sees the full set of callers (the Go backend keys on every FuncDecl).
	for _, s := range syms {
		if s.Kind == KindFunc || s.Kind == KindMethod {
			if _, ok := out[s.Name]; !ok {
				out[s.Name] = nil
			}
		}
	}
	for _, r := range refs {
		if owner := innermostFunc(syms, r.Span.StartLine); owner != "" {
			out[owner] = append(out[owner], r.Name)
		}
	}
	return out
}

// innermostFunc returns the name of the innermost def/method whose span contains
// line, or "" if none does. "Innermost" = the containing function with the latest
// (largest) start line, which for nested defs is the deepest one.
func innermostFunc(syms []Symbol, line int) string {
	best := ""
	bestStart := -1
	for _, s := range syms {
		if s.Kind != KindFunc && s.Kind != KindMethod {
			continue
		}
		if line >= s.Span.StartLine && line <= s.Span.EndLine && s.Span.StartLine > bestStart {
			best = s.Name
			bestStart = s.Span.StartLine
		}
	}
	return best
}

// nearestClass returns the name of the nearest enclosing class block on the stack,
// or "" if none. Used to decide whether a def is a method (its receiver being the
// class name).
func nearestClass(syms []Symbol, stack []pyBlock) string {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].isClass {
			return syms[stack[i].idx].Name
		}
	}
	return ""
}

// leadingIndent measures the indentation width of a line, counting a tab as one
// column. Exact tab-vs-space width doesn't matter for our purposes — we only ever
// compare two indents for "deeper / same / shallower", and consistent counting
// preserves that ordering for the conventional (don't-mix) Python file.
func leadingIndent(s string) int {
	n := 0
	for _, r := range s {
		if r == ' ' || r == '\t' {
			n++
			continue
		}
		break
	}
	return n
}

// stripPyComment removes a trailing `#` comment, respecting string quoting so a `#`
// inside a string literal is not treated as a comment start. It does not attempt to
// span multi-line strings — an acceptable approximation per the package note.
func stripPyComment(s string) string {
	inStr := false
	var quote rune
	prevBackslash := false
	for i, r := range s {
		switch {
		case inStr:
			if r == quote && !prevBackslash {
				inStr = false
			}
		case r == '\'' || r == '"':
			inStr = true
			quote = r
		case r == '#':
			return s[:i]
		}
		prevBackslash = r == '\\' && !prevBackslash
	}
	return s
}

// blankPyStrings replaces the contents of string literals with spaces so the call
// scanner never sees parentheses or identifiers that live inside quotes. Quotes
// themselves become spaces too; length is preserved so column positions are stable
// (we only use line numbers, but stable lengths keep the regex offsets sane).
func blankPyStrings(s string) string {
	out := []rune(s)
	inStr := false
	var quote rune
	prevBackslash := false
	for i, r := range out {
		if inStr {
			if r == quote && !prevBackslash {
				inStr = false
			}
			out[i] = ' '
			prevBackslash = r == '\\' && !prevBackslash
			continue
		}
		if r == '\'' || r == '"' {
			inStr = true
			quote = r
			out[i] = ' '
			prevBackslash = false
			continue
		}
		prevBackslash = false
	}
	return string(out)
}
