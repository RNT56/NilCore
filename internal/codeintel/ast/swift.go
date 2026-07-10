// Pure-Go Swift backend (Phase 13, Tier-2). Like the other brace-family backends, this
// is a deliberately lightweight, brace-aware line scanner over the standard library
// (bufio/regexp/strings) — NOT a full grammar. The goal is to reliably surface the
// structure a code-intel index needs for *typical* Swift — top-level and member
// functions (`func name(...)`), initializers (`init(...)`), class/struct/enum/protocol/
// extension/actor types, members carrying the enclosing type as their receiver, the line
// spans of those bodies, and the names they call — not to validate or fully model the
// language.
//
// Span model: Swift bodies are brace-delimited (see brace.go). A class/struct/enum/
// protocol/actor declaration opens a receiver block so the `func`s inside become methods
// on that type; an `extension Foo` opens a receiver block whose name is `Foo`, so members
// added in the extension carry `Foo` as their Recv (a symbol-less block, since the type
// is declared elsewhere — mirroring Rust's `impl`). A top-level `func` is a function.
//
// Honest scope (heuristic, like rust.go): attributes (`@objc`, `@MainActor`), generics
// (`func f<T>`), `static`/`class`/`mutating`/`override`/access modifiers, external/
// internal argument labels (`func move(to point: P)`), `throws`/`async`/`rethrows`, and
// `where` clauses are tolerated, but full resolution — operator methods, subscripts,
// protocol-extension defaults, conditional conformances — is the LSP seam's job. An
// `init?`/`init!` failable initializer is captured as `init`. Pathological inputs (a `{`
// on a later line, multi-line string literals `"""..."""`) may be approximated.
package ast

import (
	"bufio"
	"fmt"
	"regexp"
)

var (
	// Attributes (`@objc`, `@MainActor`, `@available(iOS 13, *)`) may prefix a declaration;
	// we tolerate any number of them (each optionally carrying a parenthesized argument).
	swiftAttrs = `(?:@[A-Za-z_][A-Za-z0-9_]*(?:\([^)]*\))?\s+)*`
	swiftMods  = swiftAttrs + `(?:public\s+|private\s+|fileprivate\s+|internal\s+|open\s+|final\s+|static\s+|class\s+|mutating\s+|nonmutating\s+|override\s+|required\s+|convenience\s+|lazy\s+|weak\s+|unowned\s+|dynamic\s+|optional\s+|indirect\s+)*`

	// A type declaration: `class`/`struct`/`enum`/`protocol`/`actor NAME`. The name stops
	// before a generic `<`, a conformance `:`, or the opening brace. (`class` also appears
	// as a method modifier — `class func` — but that is matched by swiftFuncRe first via
	// scan precedence, and a real type decl is `class NAME` with NAME not being `func`.)
	swiftTypeRe = regexp.MustCompile(`^\s*` + swiftMods + `(?:class|struct|enum|protocol|actor)\s+([A-Za-z_][A-Za-z0-9_]*)`)

	// `extension Foo` (optionally `extension Foo: Proto` / `extension Foo where ...`). A
	// receiver container whose name is the extended type. The name stops before a `:`, a
	// `where`, or the brace.
	swiftExtensionRe = regexp.MustCompile(`^\s*extension\s+([A-Za-z_][A-Za-z0-9_.]*)`)

	// A function header: `func NAME(`, with an optional generic `<...>` between `func` and
	// the name. Swift method/operator names can include some symbols, but we key on the
	// common identifier form.
	swiftFuncRe = regexp.MustCompile(`^\s*` + swiftMods + `func\s+(?:<[^>]*>\s*)?([A-Za-z_][A-Za-z0-9_]*)\s*[<(]`)

	// An initializer: `init(` / `init?(` / `init!(` / `init<T>(`. Captured under the name
	// `init`.
	swiftInitRe = regexp.MustCompile(`^\s*` + swiftMods + `(init)[?!]?\s*(?:<[^>]*>\s*)?\(`)

	// Call sites: identifier optionally dotted (obj.method, self.helper) before "(".
	swiftCallRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\(`)

	swiftKeyword = map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "catch": true,
		"guard": true, "return": true, "throw": true, "repeat": true, "func": true,
		"do": true, "else": true, "case": true, "init": true, "super": true,
		"self": true, "where": true, "in": true, "as": true, "is": true, "try": true,
		"await": true, "defer": true,
	}
)

// swiftParser scans Swift source line-by-line. It is stateless across calls.
type swiftParser struct{}

var _ languageParser = swiftParser{}

func (swiftParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := swiftScan(path, false)
	return syms, err
}

func (swiftParser) references(path string) ([]Reference, error) {
	_, refs, err := swiftScan(path, true)
	return refs, err
}

func (swiftParser) calls(path string) (map[string][]string, error) {
	syms, refs, err := swiftScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// swiftScan is the shared single pass; see jsScan for the structure. Header precedence: a
// type opens a receiver block; an `extension Foo` opens a symbol-less receiver block
// named `Foo`; a `func` or `init` is a method when nested inside a receiver, else a
// function (a top-level `func`).
func swiftScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := openSource(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open swift file: %w", err)
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
		case matchHeader(swiftExtensionRe, code) != "":
			// `extension Foo` — a receiver container for the type `Foo` (declared
			// elsewhere). A symbol-less block carrying the extended type's name; nested
			// members read it as their Recv. We keep the last `.`-segment so
			// `extension Swift.String` yields "String".
			stack = append(stack, braceBlock{idx: noSym, openDepth: depthBefore, isRecv: true, recvName: swiftLastSegment(matchHeader(swiftExtensionRe, code))})

		case matchHeader(swiftTypeRe, code) != "":
			name := matchHeader(swiftTypeRe, code)
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore, isRecv: true})

		case matchHeader(swiftFuncRe, code) != "":
			kind, recv := KindFunc, ""
			if r := nearestRecv(syms, stack); r != "" {
				kind, recv = KindMethod, r
			}
			pushBraceFunc(&syms, &stack, path, lineNo, matchHeader(swiftFuncRe, code), recv, kind, depthBefore)
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, swiftScanCalls)

		case matchHeader(swiftInitRe, code) != "":
			// An initializer is a member; its receiver is the enclosing type.
			pushBraceFunc(&syms, &stack, path, lineNo, "init", nearestRecv(syms, stack), KindMethod, depthBefore)
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, swiftScanCalls)

		default:
			if wantRefs {
				refs = append(refs, swiftScanCalls(code, path, lineNo)...)
			}
		}

		depth += delta
		closeBraceBlocks(&stack, depth)
		st = nextSt
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan swift file: %w", err)
	}
	return syms, refs, nil
}

// swiftLastSegment returns the final `.`-separated segment of an extended type name
// (`Swift.String` -> `String`).
func swiftLastSegment(name string) string {
	if i := lastDot(name); i >= 0 {
		return name[i+1:]
	}
	return name
}

// lastDot returns the index of the last '.' in s, or -1.
func lastDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

// swiftScanCalls extracts call references from one stripped line, dropping keyword
// matches and keeping the trailing simple name for dotted calls (obj.method() ->
// "method").
func swiftScanCalls(code, path string, lineNo int) []Reference {
	var out []Reference
	for _, m := range swiftCallRe.FindAllStringSubmatch(code, -1) {
		name := trailingName(m[1])
		if swiftKeyword[name] {
			continue
		}
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}
