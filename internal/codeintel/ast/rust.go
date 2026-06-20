// Pure-Go Rust backend (R2). Like the Python and JS backends, this is a deliberately
// lightweight, brace-aware line scanner over the standard library (bufio/regexp/
// strings) — NOT a full grammar. The goal is to reliably surface the structure a
// code-intel index needs for *typical* Rust — free `fn`s, `struct`/`enum`/`trait`
// declarations, methods (an `fn` inside an `impl` block, whose receiver is the impl'd
// type), their line spans, and the names they call — not to validate or fully model
// the language. Pathological inputs (macros that emit items, raw strings r#"..."#,
// lifetimes mistaken for char literals, trait bounds spanning lines) may be
// approximated; that is an accepted trade for staying stdlib-only and cgo-free.
//
// Span model: Rust bodies are brace-delimited, so a header spans from its line to the
// line of its matching close brace, tracked by net brace depth over comment/string-
// stripped text (see brace.go). `impl TYPE { ... }` opens a receiver block so the
// `fn`s nested inside it become methods on TYPE.
package ast

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Header patterns. Visibility (`pub`, `pub(crate)`) and `async`/`unsafe`/`const`/
// `extern` qualifiers are accepted (non-capturing) before the keyword so the name
// capture is never shifted.
var (
	// `fn NAME(` — a free function or, when inside an impl block, a method. Generics
	// (`fn name<T>(`) are tolerated: the name capture stops before `<` or `(`.
	rustFnRe = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:const\s+|async\s+|unsafe\s+|extern\s+(?:"[^"]*"\s+)?)*fn\s+([A-Za-z_][A-Za-z0-9_]*)`)
	// `struct NAME` / `enum NAME` / `trait NAME` / `union NAME` — a type declaration.
	rustTypeRe = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:struct|enum|trait|union)\s+([A-Za-z_][A-Za-z0-9_]*)`)
	// `impl ... TYPE {` opens a receiver block. We capture the LAST path segment of the
	// implemented type, handling both `impl Type` and `impl Trait for Type` (the type
	// after `for` wins; without `for`, the only type is the receiver). Generics and
	// where-clauses after the name are ignored.
	rustImplForRe = regexp.MustCompile(`^\s*impl\b.*\bfor\s+([A-Za-z_][A-Za-z0-9_:]*)`)
	// `impl\b\s*` (word-bounded, space optional) so the no-space generic forms
	// `impl<T> Type`, `impl<'a> Type`, `impl<T: Bound> Type` match as receiver blocks
	// too — not just the spaced `impl Type` — while `implicit_fn` is not mistaken for it.
	rustImplRe = regexp.MustCompile(`^\s*impl\b\s*(?:<[^>]*>\s*)?([A-Za-z_][A-Za-z0-9_:]*)`)
	// Call sites: an identifier or path (a::b::c) or selector (obj.method) immediately
	// followed by "(". We keep the trailing simple name as the callee.
	rustCallRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*(?:(?:::|\.)[A-Za-z_][A-Za-z0-9_]*)*)\s*\(`)
	// Keywords that take a "(" but are control flow / operators, not calls.
	rustKeyword = map[string]bool{
		"if": true, "match": true, "while": true, "for": true, "fn": true,
		"let": true, "return": true, "impl": true, "loop": true, "else": true,
		"move": true, "as": true, "in": true, "where": true, "use": true,
		"mod": true, "struct": true, "enum": true, "trait": true, "type": true,
		"const": true, "static": true, "ref": true, "dyn": true, "unsafe": true,
		"async": true, "await": true, "yield": true, "break": true, "continue": true,
	}
)

// rustParser scans Rust source line-by-line. It is stateless across calls.
type rustParser struct{}

func (rustParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := rustScan(path, false)
	return syms, err
}

func (rustParser) references(path string) ([]Reference, error) {
	_, refs, err := rustScan(path, true)
	return refs, err
}

func (rustParser) calls(path string) (map[string][]string, error) {
	syms, refs, err := rustScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// rustScan is the shared single pass; see jsScan for the structure (it mirrors that
// backend, differing only in header shapes and the receiver being an `impl` block).
func rustScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := os.Open(path) //nolint:gosec // path is supplied by the worktree-confined walker
	if err != nil {
		return nil, nil, fmt.Errorf("open rust file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	var syms []Symbol
	var refs []Reference
	var stack []braceBlock
	var st stripState
	depth := 0

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		code, nextSt := stripLine(raw, st)
		depthBefore := depth
		delta := braceDelta(code)

		extendBraceSpans(syms, stack, lineNo)

		switch {
		case rustImplName(code) != "":
			// `impl` is a receiver container, not a symbol of its own (the struct/enum/
			// trait it implements is declared elsewhere) — so we open a symbol-less
			// block carrying the implemented type's name, and nested fns read it as their
			// receiver. Emitting a duplicate KindType here would double-count the type.
			stack = append(stack, braceBlock{idx: noSym, openDepth: depthBefore, isRecv: true, recvName: rustImplName(code)})

		case matchHeader(rustTypeRe, code) != "":
			name := matchHeader(rustTypeRe, code)
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore})

		case matchHeader(rustFnRe, code) != "":
			kind, recv := KindFunc, ""
			if r := nearestRecv(syms, stack); r != "" {
				kind, recv = KindMethod, r
			}
			pushBraceFunc(&syms, &stack, path, lineNo, matchHeader(rustFnRe, code), recv, kind, depthBefore)
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, rustScanCalls)

		default:
			if wantRefs {
				refs = append(refs, rustScanCalls(code, path, lineNo)...)
			}
		}

		depth += delta
		closeBraceBlocks(&stack, depth)
		st = nextSt
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan rust file: %w", err)
	}
	return syms, refs, nil
}

// rustImplName returns the receiver type of an `impl` line (the type after `for` when
// present, else the implemented type), or "" if the line is not an impl header. It
// returns the last `::` segment so `impl crate::Foo` yields "Foo".
func rustImplName(code string) string {
	if m := rustImplForRe.FindStringSubmatch(code); m != nil {
		return rustLastSegment(m[1])
	}
	if m := rustImplRe.FindStringSubmatch(code); m != nil {
		return rustLastSegment(m[1])
	}
	return ""
}

// rustLastSegment returns the final `::`-separated segment of a path.
func rustLastSegment(path string) string {
	if i := strings.LastIndex(path, "::"); i >= 0 {
		return path[i+2:]
	}
	return path
}

// rustScanCalls extracts call references from one stripped line. It normalizes Rust's
// `::` path separator to `.` so the shared trailingName helper yields the final
// segment, drops keyword matches, and keeps the trailing simple name (path::call ->
// "call", obj.method -> "method"), mirroring the Go/Python/JS backends.
func rustScanCalls(code, path string, lineNo int) []Reference {
	var out []Reference
	for _, m := range rustCallRe.FindAllStringSubmatch(code, -1) {
		name := trailingName(strings.ReplaceAll(m[1], "::", "."))
		if rustKeyword[name] {
			continue
		}
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}
