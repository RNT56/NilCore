// Pure-Go C# backend (Phase 13). Like the other brace-family backends, this is a
// deliberately lightweight, brace-aware line scanner over the standard library
// (bufio/regexp/strings) — NOT a full grammar. The goal is to reliably surface the
// structure a code-intel index needs for *typical* C# — namespaces; classes,
// interfaces, structs, enums and records as types; methods (generics, `async`,
// attributes, expression-bodied `=>`) and properties (`Type Name { get; set; }`) with
// the enclosing type as the receiver; the line spans of those bodies; and the names
// they call — not to validate or fully model the language.
//
// Span model: C# bodies are brace-delimited (see brace.go). A type opens a receiver
// block so its methods/properties carry it as Recv; nested types nest their own blocks.
// A file-scoped namespace (`namespace Foo;` with no braces) is recorded as a type but
// opens no block (its members live at file scope).
//
// Honest scope (heuristic, like rust.go): attributes (`[Attr]`), generic constraints
// (`where T : ...`), expression-bodied members (`int X => 1;`), and multi-line headers
// are tolerated, but full resolution — overloads, partial classes, the precise
// method-vs-property-vs-field distinction in every case — is the LSP seam's job.
// Expression-bodied methods/properties without a `{` body are captured as single-line
// symbols. Pathological inputs (a `{` opening on a later line, verbatim/interpolated
// strings spanning lines) may be approximated.
package ast

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"
)

var (
	csMods = `(?:public\s+|private\s+|protected\s+|internal\s+|static\s+|sealed\s+|abstract\s+|partial\s+|readonly\s+|virtual\s+|override\s+|new\s+|extern\s+|unsafe\s+|file\s+)*`

	// `namespace NAME` — block-scoped (`{`) or file-scoped (`;`). Captures the
	// (possibly dotted) namespace name; we keep the trailing simple segment.
	csNamespaceRe = regexp.MustCompile(`^\s*namespace\s+([A-Za-z_][A-Za-z0-9_.]*)`)

	// A type declaration: `class`/`interface`/`struct`/`enum`/`record` (incl.
	// `record class`/`record struct`). The name stops before a generic `<`, a base
	// clause `:`, a primary-constructor `(` (records), or the brace.
	csTypeRe = regexp.MustCompile(`^\s*` + csMods + `(?:class|interface|struct|enum|record(?:\s+class|\s+struct)?)\s+([A-Za-z_][A-Za-z0-9_]*)`)

	// A method header: modifiers, an optional return type, the method name, a generic
	// `<...>` maybe, then `(params)` and either a `{` body or an expression-bodied `=>`.
	// Only meaningful inside a type. The name is the identifier just before the param
	// list's `(`.
	csMethodRe = regexp.MustCompile(`^\s*` + csMods + `(?:async\s+)?(?:[A-Za-z_][A-Za-z0-9_<>,.\[\]\?\s]*\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*(?:<[^>(]*>)?\s*\([^;]*\)\s*(?:where[^={]*)?(?:\{|=>)`)

	// A property: `Type NAME { ` followed by accessors (`get`/`set`/`init`). We match the
	// name before the `{` and require a `get`/`set`/`init` to follow on the line, OR an
	// expression-bodied property `Type NAME => expr;`. Only meaningful inside a type.
	csPropertyRe     = regexp.MustCompile(`^\s*` + csMods + `(?:[A-Za-z_][A-Za-z0-9_<>,.\[\]\?]*\s+)+([A-Za-z_][A-Za-z0-9_]*)\s*\{\s*(?:get|set|init)\b`)
	csPropertyExprRe = regexp.MustCompile(`^\s*` + csMods + `(?:[A-Za-z_][A-Za-z0-9_<>,.\[\]\?]*\s+)+([A-Za-z_][A-Za-z0-9_]*)\s*=>`)

	// Call sites: identifier optionally dotted (obj.Method, this.Helper) before "(".
	csCallRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\(`)

	csKeyword = map[string]bool{
		"if": true, "for": true, "foreach": true, "while": true, "switch": true,
		"catch": true, "lock": true, "using": true, "return": true, "throw": true,
		"new": true, "base": true, "this": true, "nameof": true, "typeof": true,
		"sizeof": true, "default": true, "checked": true, "unchecked": true,
		"do": true, "else": true, "case": true, "fixed": true, "await": true,
		"when": true, "stackalloc": true,
	}
)

// csharpParser scans C# source line-by-line. It is stateless across calls.
type csharpParser struct{}

var _ languageParser = csharpParser{}

func (csharpParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := csharpScan(path, false)
	return syms, err
}

func (csharpParser) references(path string) ([]Reference, error) {
	_, refs, err := csharpScan(path, true)
	return refs, err
}

func (csharpParser) calls(path string) (map[string][]string, error) {
	syms, refs, err := csharpScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// csharpScan is the shared single pass; see jsScan for the structure. Header precedence:
// namespace and type open receiver blocks; a property (recognized by `{ get`/`set`/
// `init` or an expression body) is checked before a method so `Type Name { get; }` is
// not mistaken for a method; a method otherwise. Properties and types only count inside
// the right scope so a control-flow header in a method body never matches.
func csharpScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := openSource(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open csharp file: %w", err)
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
		case matchHeader(csNamespaceRe, code) != "":
			name := csLastSegment(matchHeader(csNamespaceRe, code))
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			// Block-scoped namespace opens a receiver block; file-scoped (`;`, no `{`) does
			// not — its types live at file scope.
			if strings.Contains(code, "{") {
				stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore, isRecv: true})
			}

		case matchHeader(csTypeRe, code) != "":
			name := matchHeader(csTypeRe, code)
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore, isRecv: true})

		case insideCsType(stack) && (matchHeader(csPropertyRe, code) != "" || matchHeader(csPropertyExprRe, code) != ""):
			name := matchHeader(csPropertyRe, code)
			if name == "" {
				name = matchHeader(csPropertyExprRe, code)
			}
			// A property is a member, not a callable in the call graph; emit it as a
			// method-kind symbol (it has a body/accessors and an enclosing receiver),
			// matching how the index treats class members. Open a block only if its body
			// braces open on this line (auto-properties on one line do not nest).
			syms = append(syms, Symbol{Name: name, Kind: KindMethod, Recv: nearestRecv(syms, stack), Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			if strings.Contains(code, "{") {
				stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore})
			}
			// An expression-bodied property (`X => Compute(0);`) may call functions; scan
			// the part after the `=>` so those references are attributed to the property.
			if idx := strings.Index(code, "=>"); idx >= 0 && wantRefs {
				refs = append(refs, csharpScanCalls(code[idx+2:], path, lineNo)...)
			}

		case insideCsType(stack) && matchHeader(csMethodRe, code) != "" && !csKeyword[matchHeader(csMethodRe, code)]:
			name := matchHeader(csMethodRe, code)
			pushBraceFunc(&syms, &stack, path, lineNo, name, nearestRecv(syms, stack), KindMethod, depthBefore)
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, csharpScanCalls)
			// An expression-bodied method (`=>` with no `{`) opened no block; its span is
			// the single line, which is already correct.

		default:
			if wantRefs {
				refs = append(refs, csharpScanCalls(code, path, lineNo)...)
			}
		}

		depth += delta
		closeBraceBlocks(&stack, depth)
		st = nextSt
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan csharp file: %w", err)
	}
	return syms, refs, nil
}

// insideCsType reports whether the innermost open receiver block is a namespace/type —
// the gate for treating a header as a method or property.
func insideCsType(stack []braceBlock) bool {
	if len(stack) == 0 {
		return false
	}
	return stack[len(stack)-1].isRecv
}

// csLastSegment returns the final dotted segment of a namespace name (`A.B.C` -> `C`).
func csLastSegment(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// csharpScanCalls extracts call references from one stripped line, dropping keyword
// matches and keeping the trailing simple name for dotted calls (obj.Method() ->
// "Method").
func csharpScanCalls(code, path string, lineNo int) []Reference {
	var out []Reference
	for _, m := range csCallRe.FindAllStringSubmatch(code, -1) {
		name := trailingName(m[1])
		if csKeyword[name] {
			continue
		}
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}
