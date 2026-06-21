// Pure-Go Java backend (Phase 13). Like the JS and Rust backends, this is a
// deliberately lightweight, brace-aware line scanner over the standard library
// (bufio/regexp/strings) — NOT a full grammar. The goal is to reliably surface the
// structure a code-intel index needs for *typical* Java — classes, interfaces, enums,
// records and annotation types (`@interface`); their methods and constructors (with the
// enclosing type as the receiver); the line spans of those bodies; and the names they
// call — not to validate or fully model the language.
//
// Span model: Java bodies are brace-delimited, so a header spans from its line to the
// line of its matching close brace, tracked by net brace depth over comment/string-
// stripped text (see brace.go). A class/interface/enum/record/annotation opens a
// receiver block, so the methods nested inside become methods on that type, and nested
// (inner) classes nest their own receiver blocks.
//
// Honest scope (heuristic, like rust.go): generics (`<T extends ...>`), `throws`
// clauses, annotations on declarations, and multi-line headers are tolerated, but full
// resolution — overload sets, qualified type names, the difference between a field
// initializer call and a method call — is the LSP seam's job. Pathological inputs
// (annotations with parenthesized arguments on their own line, text blocks `"""..."""`,
// a `{` opening on a later line than the header) may be approximated.
package ast

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
)

// Header patterns. Modifiers (`public`/`private`/`protected`/`static`/`final`/
// `abstract`/`sealed`/`non-sealed`/`strictfp`/`default`) are accepted (non-capturing)
// before each keyword so the name capture is never shifted by a modifier.
var (
	javaMods = `(?:public\s+|private\s+|protected\s+|static\s+|final\s+|abstract\s+|sealed\s+|non-sealed\s+|strictfp\s+|default\s+)*`

	// A type declaration: `class NAME`, `interface NAME`, `enum NAME`, `record NAME`,
	// or `@interface NAME` (an annotation type). The name capture stops before a
	// generic `<`, an `extends`/`implements`/`permits` clause, a `(` (record header),
	// or the opening brace.
	javaTypeRe = regexp.MustCompile(`^\s*` + javaMods + `(?:class|interface|enum|record|@interface)\s+([A-Za-z_$][A-Za-z0-9_$]*)`)

	// A method or constructor header. We require a `(` on the line (so a field decl
	// with an initializer does not match) and capture the identifier immediately before
	// it. Modifiers, annotations already stripped to the line start, a return type and
	// generics may precede the name; the `[A-Za-z_$][A-Za-z0-9_$]*` just before `(` is
	// the method/constructor name. Constructors have no return type but match the same
	// shape (their name is the enclosing type). Only meaningful while inside a type
	// body — the scanner gates on an open receiver block.
	javaMethodRe = regexp.MustCompile(`^\s*` + javaMods + `(?:<[^>]*>\s*)?(?:[A-Za-z_$][A-Za-z0-9_$.<>,\[\]\s]*\s+)?([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)

	// Call sites: an identifier (optionally dotted, e.g. obj.method or this.helper)
	// immediately followed by "(". We keep the trailing simple name as the callee,
	// mirroring the JS/Rust backends' selector handling.
	javaCallRe = regexp.MustCompile(`([A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)*)\s*\(`)

	// Keywords that take a "(" but are control flow / operators, not calls.
	javaKeyword = map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "catch": true,
		"synchronized": true, "new": true, "return": true, "throw": true,
		"super": true, "this": true, "assert": true, "instanceof": true,
		"do": true, "else": true, "case": true, "try": true, "finally": true,
		"yield": true,
	}
)

// javaParser scans Java source line-by-line. It is stateless across calls.
type javaParser struct{}

var _ languageParser = javaParser{}

func (javaParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := javaScan(path, false)
	return syms, err
}

func (javaParser) references(path string) ([]Reference, error) {
	_, refs, err := javaScan(path, true)
	return refs, err
}

func (javaParser) calls(path string) (map[string][]string, error) {
	syms, refs, err := javaScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// javaScan is the shared single pass; see jsScan for the structure (it mirrors that
// backend, differing only in header shapes — a Java type opens a receiver block and the
// methods/constructors inside it carry that type as their receiver).
func javaScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := os.Open(path) //nolint:gosec // path is supplied by the worktree-confined walker
	if err != nil {
		return nil, nil, fmt.Errorf("open java file: %w", err)
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
		case matchHeader(javaTypeRe, code) != "":
			name := matchHeader(javaTypeRe, code)
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			// A type is a receiver container: nested methods read it as their Recv, and
			// inner classes nest their own block above it.
			stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore, isRecv: true})

		case insideType(stack) && matchHeader(javaMethodRe, code) != "" && !javaKeyword[matchHeader(javaMethodRe, code)]:
			name := matchHeader(javaMethodRe, code)
			pushBraceFunc(&syms, &stack, path, lineNo, name, nearestRecv(syms, stack), KindMethod, depthBefore)
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, javaScanCalls)

		default:
			if wantRefs {
				refs = append(refs, javaScanCalls(code, path, lineNo)...)
			}
		}

		depth += delta
		closeBraceBlocks(&stack, depth)
		st = nextSt
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan java file: %w", err)
	}
	return syms, refs, nil
}

// insideType reports whether the innermost open block is a type (class/interface/enum/
// record/annotation) receiver block — the condition for treating a `NAME(...) {` header
// as a method/constructor. Java has no free functions, so a method only counts inside a
// type body; this prevents a control-flow header inside a method body from matching.
func insideType(stack []braceBlock) bool {
	if len(stack) == 0 {
		return false
	}
	return stack[len(stack)-1].isRecv
}

// javaScanCalls extracts call references from one stripped line, dropping keyword
// matches and keeping the trailing simple name for dotted calls (obj.method() ->
// "method"), mirroring the JS/Rust backends.
func javaScanCalls(code, path string, lineNo int) []Reference {
	var out []Reference
	for _, m := range javaCallRe.FindAllStringSubmatch(code, -1) {
		name := trailingName(m[1])
		if javaKeyword[name] {
			continue
		}
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}
