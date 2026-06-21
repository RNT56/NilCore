// Pure-Go C++ backend (Phase 13). Like the other brace-family backends, this is a
// deliberately lightweight, brace-aware line scanner over the standard library
// (bufio/regexp/strings) — NOT a full grammar. It extends the C shapes (free functions,
// struct/union/enum/typedef) with the C++ ones a code-intel index needs for *typical*
// C++:
//
//   - `class`/`struct`/`namespace` declarations as type containers (a receiver block,
//     so methods declared/defined in their body carry the type/namespace as Recv);
//   - in-class method definitions (`void Foo::bar` written inside the body is rare; the
//     common in-body form `ReturnType name(...) {` reads its Recv from the enclosing
//     class);
//   - out-of-line definitions `ReturnType Type::method(...) {` whose Recv is the `Type`
//     before `::`;
//   - constructors / destructors (`Type::Type`, `Type::~Type`) and `operator` overloads;
//   - `template<...>` prefixes, which attach to the following declaration (we simply
//     skip a lone `template<...>` line and let the next line's header match).
//
// Span model: bodies are brace-delimited (see brace.go), tracked by net brace depth over
// comment/string-stripped text.
//
// Honest scope (heuristic, like rust.go): this is NOT a C++ parser. Full template and
// macro resolution, overload sets, qualified-name lookup, ADL, and nested template
// arguments are the LSP seam's job. Multi-line headers, a `{` opening on a later line
// than the header, raw string literals `R"(...)"`, and attribute syntax `[[...]]` may be
// approximated. The Recv for an out-of-line definition is the last qualifier before the
// final `::` (so `ns::Type::method` yields `Type`); deeper nesting is collapsed to that
// final qualifier.
package ast

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	cppMods = `(?:static\s+|inline\s+|virtual\s+|explicit\s+|constexpr\s+|consteval\s+|friend\s+|extern\s+|public:\s*|private:\s*|protected:\s*)*`

	// `class NAME` / `struct NAME` — a type container (receiver block). The name stops
	// before a base-clause `:`, a generic `<`, or the opening brace. (A bare `struct
	// NAME { ... }` C-style aggregate also matches; treating it as a receiver is
	// harmless — if no methods nest, no Recv is produced.)
	cppClassRe = regexp.MustCompile(`^\s*(?:template\s*<[^>]*>\s*)?(?:class|struct)\s+([A-Za-z_][A-Za-z0-9_]*)`)

	// `namespace NAME {` — a named namespace container. Anonymous namespaces (no name)
	// are skipped (no symbol, but their braces still affect depth via braceDelta).
	cppNamespaceRe = regexp.MustCompile(`^\s*namespace\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{`)

	// `enum NAME` / `enum class NAME` / `union NAME` — a flat type (no nesting we track).
	cppEnumRe = regexp.MustCompile(`^\s*(?:typedef\s+)?(?:enum(?:\s+class|\s+struct)?|union)\s+([A-Za-z_][A-Za-z0-9_]*)`)

	// An OUT-OF-LINE definition `... Qual::name(params) {` — captures the qualifier
	// (Recv) and the method name. Handles destructors (`Qual::~name`), operators
	// (`Qual::operator==`), and a templated prefix. The qualifier is the segment right
	// before the final `::`.
	cppOutOfLineRe = regexp.MustCompile(`^\s*(?:template\s*<[^>]*>\s*)?` + cppMods + `(?:[A-Za-z_][A-Za-z0-9_:<>,*&\s]*\s+|\*\s*)?([A-Za-z_][A-Za-z0-9_]*)::(~?[A-Za-z_][A-Za-z0-9_]*|operator\s*\S+)\s*\([^;{]*\)\s*(?:const\s*)?(?:noexcept\s*)?(?:override\s*)?\{`)

	// An IN-CLASS method/constructor/destructor definition `ReturnType name(params) {`
	// (or a constructor/destructor with no return type). Only meaningful inside a class
	// receiver block. Captures the trailing identifier (or `~name`/`operator X`) before
	// the parameter list. A trailing `: member(init)` constructor-initializer list before
	// the `{` is tolerated by allowing it before the brace.
	cppMethodRe = regexp.MustCompile(`^\s*(?:template\s*<[^>]*>\s*)?` + cppMods + `(?:[A-Za-z_][A-Za-z0-9_:<>,*&\s]*\s+|\*\s*)?(~?[A-Za-z_][A-Za-z0-9_]*|operator\s*\S+)\s*\([^;{]*\)\s*(?:const\s*)?(?:noexcept\s*)?(?:override\s*)?(?:=\s*(?:default|delete)\s*)?(?::[^{]*)?\{`)

	// A free function definition (reuse the C shape).
	cppFuncRe = regexp.MustCompile(`^\s*(?:template\s*<[^>]*>\s*)?` + cppMods + `(?:[A-Za-z_][A-Za-z0-9_:<>,*&\s]*[\s*]+)+\*?\s*([A-Za-z_][A-Za-z0-9_]*)\s*\([^;{]*\)\s*(?:noexcept\s*)?\{`)

	// Call sites: identifier optionally selected (obj.m, p->m, ns::f) before "(". Keep
	// the trailing simple name.
	cppCallRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*(?:(?:\.|->|::)[A-Za-z_][A-Za-z0-9_]*)*)\s*\(`)

	// A lone `template<...>` line (the declaration follows on the next line). We skip it
	// rather than mismatch the angle brackets as a type.
	cppTemplateOnlyRe = regexp.MustCompile(`^\s*template\s*<[^>]*>\s*$`)

	cppKeyword = map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "catch": true,
		"return": true, "sizeof": true, "new": true, "delete": true, "throw": true,
		"do": true, "else": true, "case": true, "static_cast": true, "dynamic_cast": true,
		"reinterpret_cast": true, "const_cast": true, "alignof": true, "decltype": true,
		"noexcept": true, "typeid": true, "sizeof...": true, "static_assert": true,
		"__attribute__": true, "goto": true,
	}
)

// cppParser scans C/C++ source line-by-line. It is stateless across calls.
type cppParser struct{}

var _ languageParser = cppParser{}

func (cppParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := cppScan(path, false)
	return syms, err
}

func (cppParser) references(path string) ([]Reference, error) {
	_, refs, err := cppScan(path, true)
	return refs, err
}

func (cppParser) calls(path string) (map[string][]string, error) {
	syms, refs, err := cppScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// cppScan is the shared single pass. Header precedence matters: a `class`/`struct`/
// `namespace` opens a receiver block; an out-of-line `Type::method` is recognized
// regardless of nesting (its Recv comes from the qualifier, not the stack); an in-class
// method only counts inside a class receiver; a free function otherwise.
func cppScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := os.Open(path) //nolint:gosec // path is supplied by the worktree-confined walker
	if err != nil {
		return nil, nil, fmt.Errorf("open cpp file: %w", err)
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
		case cppTemplateOnlyRe.MatchString(code):
			// A lone template prefix; the real declaration is on the next line.

		case matchHeader(cppNamespaceRe, code) != "":
			// A namespace is a SPAN container but NOT a method receiver: its members are
			// free functions, not methods (and `Type::method` out-of-line defs carry their
			// own Recv). So we open a non-isRecv block — its span extends over the body, but
			// nearestRecv skips it and insideCppClass stays false inside it.
			name := matchHeader(cppNamespaceRe, code)
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore})

		case matchHeader(cppClassRe, code) != "" && !strings.Contains(code, "(") && cppForwardOrVarException(code):
			// `class NAME` / `struct NAME` whose body opens (no `(`, not a forward decl /
			// variable). A receiver block so nested methods get this type as Recv.
			name := matchHeader(cppClassRe, code)
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore, isRecv: true})

		case cppOutOfLine(code) != nil:
			// Out-of-line definition: Recv from the qualifier before `::`.
			ool := cppOutOfLine(code)
			pushBraceFunc(&syms, &stack, path, lineNo, ool.name, ool.recv, KindMethod, depthBefore)
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, cppScanCalls)

		case matchHeader(cppEnumRe, code) != "":
			name := matchHeader(cppEnumRe, code)
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			if strings.Contains(code, "{") {
				stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore})
			}

		case insideCppClass(stack) && matchHeader(cppMethodRe, code) != "" && !cppKeyword[cppName(matchHeader(cppMethodRe, code))]:
			// In-class method/constructor/destructor; Recv from the enclosing class.
			name := cppName(matchHeader(cppMethodRe, code))
			pushBraceFunc(&syms, &stack, path, lineNo, name, nearestRecv(syms, stack), KindMethod, depthBefore)
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, cppScanCalls)

		case matchHeader(cppFuncRe, code) != "" && !cppKeyword[matchHeader(cppFuncRe, code)]:
			// A free function (no receiver).
			pushBraceFunc(&syms, &stack, path, lineNo, matchHeader(cppFuncRe, code), "", KindFunc, depthBefore)
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, cppScanCalls)

		default:
			if wantRefs {
				refs = append(refs, cppScanCalls(code, path, lineNo)...)
			}
		}

		depth += delta
		closeBraceBlocks(&stack, depth)
		st = nextSt
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan cpp file: %w", err)
	}
	return syms, refs, nil
}

// cppOutOfLineDef is a recognized out-of-line definition: the method name and its
// receiver type (the qualifier before the final `::`).
type cppOutOfLineDef struct {
	name string
	recv string
}

// cppOutOfLine returns the parsed out-of-line definition on a line, or nil. The method
// name is normalized (a destructor keeps its `~`; an operator collapses whitespace to
// "operator"+symbol).
func cppOutOfLine(code string) *cppOutOfLineDef {
	m := cppOutOfLineRe.FindStringSubmatch(code)
	if m == nil {
		return nil
	}
	return &cppOutOfLineDef{recv: m[1], name: cppName(m[2])}
}

// cppName normalizes a captured method token: it collapses the space in `operator X`
// (so `operator ==` becomes `operator==`) and otherwise returns the token unchanged
// (a destructor's leading `~` is kept).
func cppName(tok string) string {
	if strings.HasPrefix(tok, "operator") {
		return "operator" + strings.TrimSpace(strings.TrimPrefix(tok, "operator"))
	}
	return tok
}

// cppForwardOrVarException reports whether a `class`/`struct` line is a real container
// (true) rather than a forward declaration (`class Foo;`) or an inheritance-only line
// that we still want as a container. A line ending in `;` with no `{` is a forward
// declaration / variable — not a container — so we exclude it.
func cppForwardOrVarException(code string) bool {
	trimmed := strings.TrimSpace(code)
	if strings.HasSuffix(trimmed, ";") && !strings.Contains(trimmed, "{") {
		return false
	}
	return true
}

// insideCppClass reports whether the innermost open receiver block is a class/struct/
// namespace — the gate for treating a bare method header as a method.
func insideCppClass(stack []braceBlock) bool {
	if len(stack) == 0 {
		return false
	}
	return stack[len(stack)-1].isRecv
}

// cppScanCalls extracts call references from one stripped line, normalizing `->` and
// `::` selectors to `.` so trailingName yields the final segment, dropping keyword/cast
// matches.
func cppScanCalls(code, path string, lineNo int) []Reference {
	var out []Reference
	for _, m := range cppCallRe.FindAllStringSubmatch(code, -1) {
		full := strings.ReplaceAll(strings.ReplaceAll(m[1], "->", "."), "::", ".")
		name := trailingName(full)
		if cppKeyword[name] {
			continue
		}
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}
