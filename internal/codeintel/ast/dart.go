// Pure-Go Dart backend (Phase 13, Tier-3). Like the other brace-family backends, this is
// a deliberately lightweight, brace-aware line scanner over the standard library
// (bufio/regexp/strings) — NOT a full grammar. The goal is to reliably surface the
// structure a code-intel index needs for *typical* Dart — `class`/`mixin`/`enum`/
// `extension` types, methods and top-level functions, constructors, getters/setters, the
// line spans of those bodies, and the names they call — not to validate or fully model the
// language.
//
// Span model: Dart bodies are brace-delimited (see brace.go). A class/mixin/enum/extension
// opens a receiver block so the methods inside become methods on that type; a top-level
// function is a free function. A member with an `=>` arrow body and no `{` is captured as a
// single-line symbol.
//
// Constructors: `ClassName(...)` and named constructors `ClassName.named(...)` inside the
// class body match the method shape; their name is the identifier before `(`. The enclosing
// type is the receiver, so a constructor reads its own class as Recv.
//
// Getters/setters: `Type get name` / `set name(...)` declare a property accessor; we surface
// `name` as a method on the enclosing type.
//
// Honest scope (heuristic, like rust.go): annotations (`@override`), generics
// (`List<int> f()`), `async`/`async*`/`sync*`, `static`/`final`/`const`/`late`/access
// (Dart has none) modifiers, and `=>` arrow bodies are tolerated, but full resolution —
// named/optional parameters, factory constructors vs generative, mixin application — is the
// LSP seam's job. Pathological inputs (a `{` on a later line, multi-line strings) may be
// approximated.
package ast

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	// A type declaration: `class`/`mixin`/`enum`/`extension NAME`. `abstract`/`base`/
	// `final`/`sealed`/`interface`/`mixin` qualifiers are folded into the modifier set. The
	// name stops before a generic `<`, an `extends`/`implements`/`with`/`on` clause, or the
	// opening brace. (An `extension on Foo` has no name before `on`; we capture the optional
	// extension name — an unnamed extension yields no symbol but still opens a receiver via
	// dartExtensionRe below.)
	dartTypeMods = `(?:abstract\s+|base\s+|final\s+|sealed\s+|interface\s+|mixin\s+)*`
	dartTypeRe   = regexp.MustCompile(`^\s*` + dartTypeMods + `(?:class|mixin|enum)\s+([A-Za-z_$][A-Za-z0-9_$]*)`)

	// `extension NAME on Type` / `extension on Type` — a receiver container for `Type`. The
	// receiver is the type after `on` (so members read it as their Recv), captured here. An
	// unnamed extension still opens the block.
	dartExtensionRe = regexp.MustCompile(`^\s*extension\s+(?:[A-Za-z_$][A-Za-z0-9_$]*\s+)?on\s+([A-Za-z_$][A-Za-z0-9_$.]*)`)

	// A method / top-level function header: a return type (or `void`/`Future<...>`/generics)
	// MUST precede the name so a bare call statement (`helper();`) is never mistaken for a
	// declaration — the distinguishing signal between a decl and a call is the leading return
	// type. Optional `@annotations` and `static`/`final`/`const`/`external`/`abstract`
	// modifiers may come first. Group 1 is that leading return-type token (so the scanner can
	// reject statement keywords like `return`/`await` that masquerade as a type); group 2 is
	// the function/method name (immediately before `(`).
	dartFuncRe = regexp.MustCompile(`^\s*` + `(?:@[A-Za-z_$][A-Za-z0-9_$]*(?:\([^)]*\))?\s+)*` + `(?:static\s+|final\s+|const\s+|external\s+|abstract\s+|covariant\s+)*` + `([A-Za-z_$][A-Za-z0-9_$]*)[A-Za-z0-9_$<>,\[\]?.]*[?\s]\s*([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)

	// Statement keywords that can appear as the FIRST token of a body line before an
	// identifier-and-paren (`return upper()`, `await refresh()`, `throw Bad()`), which the
	// func-header regex would otherwise read as a return-type-plus-name declaration.
	dartLeadingKeyword = map[string]bool{
		"return": true, "await": true, "throw": true, "yield": true, "if": true,
		"for": true, "while": true, "switch": true, "assert": true, "rethrow": true,
		"else": true, "do": true, "in": true, "case": true,
	}

	// A constructor: `ClassName(` or a named constructor `ClassName.named(` whose leading
	// identifier equals the enclosing type. The leading optional modifiers cover `const`/
	// `factory`. We capture the whole (possibly dotted) name and let the scanner confirm the
	// class part matches the enclosing receiver before accepting it as a constructor.
	dartCtorRe = regexp.MustCompile(`^\s*(?:const\s+|factory\s+)*([A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)?)\s*\(`)

	// A getter (`Type get name`) or setter (`set name(...)`). Group 1 is the accessor name.
	// The getter has no `(`; the setter does. We surface the accessor as a method `name`.
	dartGetterRe = regexp.MustCompile(`^\s*(?:[A-Za-z_$][A-Za-z0-9_$<>,\[\]?. ]*\s+)?get\s+([A-Za-z_$][A-Za-z0-9_$]*)`)
	dartSetterRe = regexp.MustCompile(`^\s*set\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)

	// Call sites: identifier optionally dotted (obj.method, this.helper) before "(".
	dartCallRe = regexp.MustCompile(`([A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)*)\s*\(`)

	dartKeyword = map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "catch": true,
		"return": true, "throw": true, "new": true, "do": true, "else": true,
		"case": true, "try": true, "finally": true, "super": true, "this": true,
		"assert": true, "await": true, "yield": true, "rethrow": true, "get": true,
		"set": true, "with": true,
	}
)

// dartParser scans Dart source line-by-line. It is stateless across calls.
type dartParser struct{}

var _ languageParser = dartParser{}

func (dartParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := dartScan(path, false)
	return syms, err
}

func (dartParser) references(path string) ([]Reference, error) {
	_, refs, err := dartScan(path, true)
	return refs, err
}

func (dartParser) calls(path string) (map[string][]string, error) {
	syms, refs, err := dartScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// dartScan is the shared single pass; see jsScan for the structure. Header precedence: an
// extension opens a symbol-less receiver block named for the extended type; a type opens a
// receiver block; a getter/setter or a func/method/constructor is a method when nested in a
// receiver, else a free function.
func dartScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := os.Open(path) //nolint:gosec // path is supplied by the worktree-confined walker
	if err != nil {
		return nil, nil, fmt.Errorf("open dart file: %w", err)
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
		case matchHeader(dartExtensionRe, code) != "":
			// `extension [Name] on Type` — a receiver container for `Type` (declared
			// elsewhere). A symbol-less block carrying the extended type's last segment.
			stack = append(stack, braceBlock{idx: noSym, openDepth: depthBefore, isRecv: true, recvName: trailingName(matchHeader(dartExtensionRe, code))})

		case matchHeader(dartTypeRe, code) != "":
			name := matchHeader(dartTypeRe, code)
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			if strings.Contains(code, "{") {
				stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore, isRecv: true})
			}

		case matchHeader(dartGetterRe, code) != "":
			dartPushMember(&syms, &stack, &refs, path, lineNo, matchHeader(dartGetterRe, code), code, depthBefore, syms, stack, wantRefs)

		case matchHeader(dartSetterRe, code) != "":
			dartPushMember(&syms, &stack, &refs, path, lineNo, matchHeader(dartSetterRe, code), code, depthBefore, syms, stack, wantRefs)

		case dartFuncName(code) != "":
			dartPushMember(&syms, &stack, &refs, path, lineNo, dartFuncName(code), code, depthBefore, syms, stack, wantRefs)

		case insideType(stack) && dartIsConstructor(code, nearestRecv(syms, stack)):
			// A constructor `ClassName(...)` / `ClassName.named(...)` inside its class body. We
			// surface it under its full (possibly dotted) name as a member on the class.
			dartPushMember(&syms, &stack, &refs, path, lineNo, matchHeader(dartCtorRe, code), code, depthBefore, syms, stack, wantRefs)

		default:
			if wantRefs {
				refs = append(refs, dartScanCalls(code, path, lineNo)...)
			}
		}

		depth += delta
		closeBraceBlocks(&stack, depth)
		st = nextSt
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan dart file: %w", err)
	}
	return syms, refs, nil
}

// dartPushMember emits a function/method/constructor/accessor symbol named `name`. It is a
// method when nested inside a receiver block, else a free function. A brace body opens a
// span block; an `=>` arrow body (no `{`) stays a single-line symbol. Header argument calls
// after the `(` are recorded. The `synsSnapshot`/`stackSnapshot` are the pre-mutation views
// used to resolve the receiver (the slices may be re-pointed by append inside).
func dartPushMember(syms *[]Symbol, stack *[]braceBlock, refs *[]Reference, path string, lineNo int, name, code string, depthBefore int, synsSnapshot []Symbol, stackSnapshot []braceBlock, wantRefs bool) {
	kind, recv := KindFunc, ""
	if r := nearestRecv(synsSnapshot, stackSnapshot); r != "" {
		kind, recv = KindMethod, r
	}
	if strings.Contains(code, "{") {
		pushBraceFunc(syms, stack, path, lineNo, name, recv, kind, depthBefore)
	} else {
		*syms = append(*syms, Symbol{Name: name, Kind: kind, Recv: recv, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	scanHeaderCalls(refs, code, path, lineNo, wantRefs, dartScanCalls)
}

// dartFuncName returns the declared function/method name on a header line, or "" if the line
// is not a declaration. It rejects a line whose leading return-type token is actually a
// statement keyword (`return upper()`, `await refresh()`) — those are calls, not decls — and
// a name that is itself a keyword.
func dartFuncName(code string) string {
	m := dartFuncRe.FindStringSubmatch(code)
	if m == nil {
		return ""
	}
	if dartLeadingKeyword[m[1]] || dartKeyword[m[2]] {
		return ""
	}
	return m[2]
}

// dartIsConstructor reports whether a line is a constructor header for the enclosing class
// `recv`: a `ClassName(` or named `ClassName.named(` whose leading class identifier equals
// `recv`. This distinguishes a constructor (no return type) from an ordinary call statement,
// which would not match a class name. `recv` empty (not inside a named type) is never a
// constructor.
func dartIsConstructor(code, recv string) bool {
	if recv == "" {
		return false
	}
	m := matchHeader(dartCtorRe, code)
	if m == "" {
		return false
	}
	cls := m
	if dot := strings.IndexByte(cls, '.'); dot >= 0 {
		cls = cls[:dot]
	}
	return cls == recv
}

// dartScanCalls extracts call references from one stripped line, dropping keyword matches
// and keeping the trailing simple name for dotted calls (obj.method() -> "method").
func dartScanCalls(code, path string, lineNo int) []Reference {
	var out []Reference
	for _, m := range dartCallRe.FindAllStringSubmatch(code, -1) {
		name := trailingName(m[1])
		if dartKeyword[name] {
			continue
		}
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}
