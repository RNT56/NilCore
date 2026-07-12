// Pure-Go C backend (Phase 13). Like the other brace-family backends, this is a
// deliberately lightweight, brace-aware line scanner over the standard library
// (bufio/regexp/strings) — NOT a full grammar. The goal is to reliably surface the
// structure a code-intel index needs for *typical* C — function definitions,
// `struct`/`union`/`enum` type declarations, and `typedef`s; the line spans of function
// bodies; and the names those functions call — not to validate or fully model the
// language. C has no receivers, so every function is a free function (KindFunc) and no
// symbol carries a Recv.
//
// Span model: function bodies are brace-delimited, so a definition spans from its
// header line to the line of its matching close brace, tracked by net brace depth over
// comment/string-stripped text (see brace.go). A `struct`/`union`/`enum` whose body
// braces open on the header line opens a (non-receiver) block so its span covers the
// body, but it contributes no nested symbols.
//
// Honest scope (heuristic): the C preprocessor is not expanded — function-like macros,
// `#define`d signatures, and conditionally-compiled blocks are seen literally. K&R-style
// definitions (parameter declarations between the header `)` and the `{`) are tolerated
// only when the `{` is on the header line; a prototype (a declaration ending in `;`
// with no body) is correctly NOT treated as a definition. Pointer return types,
// `static`/`inline`/`extern`/`const` qualifiers, and struct-returning functions are
// handled. Full type resolution is the LSP seam's job.
package ast

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"
)

var (
	// A type-tag declaration: `struct NAME`, `union NAME`, or `enum NAME`. Captures the
	// tag name. The optional `typedef ` prefix lets `typedef struct NAME {` register the
	// tag (the trailing typedef alias, if any, is handled separately when the tag is
	// anonymous).
	cTypeRe = regexp.MustCompile(`^\s*(?:typedef\s+)?(?:struct|union|enum)\s+([A-Za-z_][A-Za-z0-9_]*)`)

	// A `typedef ... NAME;` whose alias is the last identifier before the terminating
	// `;` on the line — captures simple one-line typedefs (`typedef int Handle;`,
	// `typedef struct Foo Foo;`). Function-pointer and multi-line typedefs are an
	// accepted approximation.
	cTypedefRe = regexp.MustCompile(`^\s*typedef\b.*?\b([A-Za-z_][A-Za-z0-9_]*)\s*;`)

	// A function DEFINITION header: a return type (possibly pointer/qualified) followed
	// by `NAME(params)` and then — somewhere on the line — a `{` opening the body. The
	// `.*\{` tail requires the body to open on this line, which distinguishes a
	// definition from a prototype (`int f(void);`, no brace). The name is the last
	// identifier before the parameter list's `(`.
	cFuncRe = regexp.MustCompile(`^\s*(?:[A-Za-z_][A-Za-z0-9_]*[\s*]+)+\*?\s*([A-Za-z_][A-Za-z0-9_]*)\s*\([^;{]*\)\s*\{`)

	// Call sites: an identifier immediately followed by "(". C has no member-call dot
	// syntax that resolves to a callable name we track (a `s.fn(` field call keeps the
	// trailing name), so we accept an optional dotted/arrow selector and keep the
	// trailing simple name.
	cCallRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*(?:(?:\.|->)[A-Za-z_][A-Za-z0-9_]*)*)\s*\(`)

	// Keywords / operators that take a "(" but are not calls.
	cKeyword = map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "return": true,
		"sizeof": true, "defined": true, "do": true, "else": true, "case": true,
		"goto": true, "_Alignof": true, "_Generic": true, "alignof": true,
	}
)

// cParser scans C source line-by-line. It is stateless across calls.
type cParser struct{}

var _ languageParser = cParser{}

func (cParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := cScan(path, false)
	return syms, err
}

func (cParser) references(path string) ([]Reference, error) {
	_, refs, err := cScan(path, true)
	return refs, err
}

func (cParser) calls(path string) (map[string][]string, error) {
	syms, refs, err := cScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// cScan is the shared single pass. It mirrors the other brace backends but emits no
// receivers (C has no methods): a function header opens a plain block; a struct/union/
// enum or typedef emits a flat KindType.
func cScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := openSource(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open c file: %w", err)
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
		case matchHeader(cFuncRe, code) != "" && !cKeyword[matchHeader(cFuncRe, code)]:
			// A free function definition (no receiver).
			pushBraceFunc(&syms, &stack, path, lineNo, matchHeader(cFuncRe, code), "", KindFunc, depthBefore)
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, cScanCalls)

		case matchHeader(cTypeRe, code) != "":
			// A struct/union/enum tag. If its body braces open on this line, open a span
			// block so the type's span covers the body; otherwise it is a one-line tag.
			name := matchHeader(cTypeRe, code)
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			if strings.Contains(code, "{") {
				stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore})
			}

		case matchHeader(cTypedefRe, code) != "":
			// A one-line `typedef ... NAME;` alias.
			syms = append(syms, Symbol{Name: matchHeader(cTypedefRe, code), Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			if wantRefs {
				refs = append(refs, cScanCalls(code, path, lineNo)...)
			}

		default:
			if wantRefs {
				refs = append(refs, cScanCalls(code, path, lineNo)...)
			}
		}

		depth += delta
		closeBraceBlocks(&stack, depth)
		st = nextSt
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan c file: %w", err)
	}
	return syms, refs, nil
}

// cScanCalls extracts call references from one stripped line, dropping keyword/operator
// matches (so `return (`, `sizeof(`, `if (` never read as calls) and keeping the
// trailing simple name for member/arrow calls.
func cScanCalls(code, path string, lineNo int) []Reference {
	var out []Reference
	for _, m := range cCallRe.FindAllStringSubmatch(code, -1) {
		// Normalize `->` to `.` so trailingName yields the final selector segment.
		name := trailingName(strings.ReplaceAll(m[1], "->", "."))
		if cKeyword[name] {
			continue
		}
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}
