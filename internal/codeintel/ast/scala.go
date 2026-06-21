// Pure-Go Scala backend (Phase 13, Tier-3). Like the other brace-family backends, this
// is a deliberately lightweight, brace-aware line scanner over the standard library
// (bufio/regexp/strings) — NOT a full grammar. The goal is to reliably surface the
// structure a code-intel index needs for *typical* Scala — `def`s (methods/functions),
// `class`/`object`/`trait`/`case class`/`case object`/`enum` types, members carrying the
// enclosing type as their receiver, the line spans of those bodies, and the names they
// call — not to validate or fully model the language.
//
// Span model: Scala bodies are brace-delimited (see brace.go). A class/object/trait/enum
// opens a receiver block so the `def`s inside become methods on that type; a top-level
// `def` is a function. A `def` whose body is an `=`-expression with no `{` is captured as
// a single-line symbol (like Kotlin's `=`-body).
//
// Honest scope (heuristic, like rust.go): annotations (`@foo`), generics (`def f[T]`),
// `implicit`/`given`/`override`/`final`/`sealed`/access modifiers, and `=`-expression
// bodies are tolerated, but full resolution — overloads, implicits, given/using clauses,
// path-dependent types — is the LSP seam's job.
//
// Scala 3 optional-braces / significant-indentation syntax is NOT modeled: this backend
// matches the BRACE style (Scala 2 and brace-using Scala 3). An indentation-region body
// with no `{` will be captured as a single-line symbol (its span understated) and nested
// members inside it will read the wrong receiver — that dynamic surface is the LSP seam's
// lens, not this static scanner's. Pathological inputs (a `{` on a later line, multi-line
// string interpolations `"""..."""`) may be approximated.
package ast

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	// Modifiers accepted (non-capturing) before a type or def keyword so the name capture
	// is never shifted. `case` is folded in here so `case class`/`case object` match.
	scalaMods = `(?:private\s+|protected\s+|public\s+|final\s+|sealed\s+|abstract\s+|implicit\s+|lazy\s+|override\s+|open\s+|inline\s+|transparent\s+|case\s+)*`

	// A type declaration: `class`/`object`/`trait`/`enum NAME` (with `case` already folded
	// into the modifier set, so `case class Foo` and `case object Bar` match). The name
	// stops before a generic `[`, a primary-constructor `(`, an `extends`/`with` clause, a
	// `:`, or the opening brace.
	scalaTypeRe = regexp.MustCompile(`^\s*` + scalaMods + `(?:class|object|trait|enum)\s+([A-Za-z_$][A-Za-z0-9_$]*)`)

	// A def header: `def NAME(` / `def NAME[T](` / `def NAME =` / `def NAME: T =`. An
	// optional generic `[...]` may follow the name. We capture the identifier after `def`.
	// Scala method names can be symbolic operators, but we key on the common identifier
	// form. The presence of `(`, `[`, `:`, or `=` after the name confirms a real def.
	scalaDefRe = regexp.MustCompile(`^\s*` + scalaMods + `def\s+([A-Za-z_$][A-Za-z0-9_$]*)`)

	// `val`/`var NAME` members — surfaced as fields (KindVar/KindConst). A `val` is a
	// constant binding, a `var` a mutable one. Only captured inside a type body so a local
	// `val` inside a def is not surfaced.
	scalaValRe = regexp.MustCompile(`^\s*` + scalaMods + `(val|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)`)

	// Call sites: identifier optionally dotted (obj.method, this.helper) before "(".
	scalaCallRe = regexp.MustCompile(`([A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)*)\s*\(`)

	scalaKeyword = map[string]bool{
		"if": true, "for": true, "while": true, "match": true, "catch": true,
		"return": true, "throw": true, "new": true, "yield": true, "def": true,
		"do": true, "else": true, "case": true, "try": true, "finally": true,
		"super": true, "this": true, "with": true, "extends": true, "given": true,
		"using": true, "import": true, "package": true,
	}
)

// scalaParser scans Scala source line-by-line. It is stateless across calls.
type scalaParser struct{}

var _ languageParser = scalaParser{}

func (scalaParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := scalaScan(path, false)
	return syms, err
}

func (scalaParser) references(path string) ([]Reference, error) {
	_, refs, err := scalaScan(path, true)
	return refs, err
}

func (scalaParser) calls(path string) (map[string][]string, error) {
	syms, refs, err := scalaScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// scalaScan is the shared single pass; see jsScan for the structure. Header precedence: a
// type opens a receiver block; a `def` is a method when nested inside a type, else a free
// function; a `val`/`var` inside a type is a field.
func scalaScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := os.Open(path) //nolint:gosec // path is supplied by the worktree-confined walker
	if err != nil {
		return nil, nil, fmt.Errorf("open scala file: %w", err)
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
		case matchHeader(scalaTypeRe, code) != "":
			name := matchHeader(scalaTypeRe, code)
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			// Open a receiver block only if the body braces open on this line; a type with no
			// body (e.g. `case class P(x: Int)`) nests nothing.
			if strings.Contains(code, "{") {
				stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore, isRecv: true})
			}

		case matchHeader(scalaDefRe, code) != "":
			name := matchHeader(scalaDefRe, code)
			kind, recv := KindFunc, ""
			if r := nearestRecv(syms, stack); r != "" {
				kind, recv = KindMethod, r
			}
			// Open a block only when a brace body opens on this line; an `=`-expression body
			// (`def f = expr`) has no block and stays a single-line symbol.
			if strings.Contains(code, "{") {
				pushBraceFunc(&syms, &stack, path, lineNo, name, recv, kind, depthBefore)
			} else {
				syms = append(syms, Symbol{Name: name, Kind: kind, Recv: recv, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			}
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, scalaScanCalls)

		case insideType(stack) && scalaValMatch(code) != nil:
			v := scalaValMatch(code)
			syms = append(syms, Symbol{Name: v.name, Kind: v.kind, Recv: nearestRecv(syms, stack), Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			if wantRefs {
				refs = append(refs, scalaScanCalls(code, path, lineNo)...)
			}

		default:
			if wantRefs {
				refs = append(refs, scalaScanCalls(code, path, lineNo)...)
			}
		}

		depth += delta
		closeBraceBlocks(&stack, depth)
		st = nextSt
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan scala file: %w", err)
	}
	return syms, refs, nil
}

// scalaVal is a recognized `val`/`var` member: its name and the kind it maps to (a `val`
// is a const binding, a `var` a mutable one).
type scalaVal struct {
	name string
	kind Kind
}

// scalaValMatch parses a `val`/`var` member header, or returns nil.
func scalaValMatch(code string) *scalaVal {
	m := scalaValRe.FindStringSubmatch(code)
	if m == nil {
		return nil
	}
	kind := KindVar
	if m[1] == "val" {
		kind = KindConst
	}
	return &scalaVal{name: m[2], kind: kind}
}

// scalaScanCalls extracts call references from one stripped line, dropping keyword matches
// and keeping the trailing simple name for dotted calls (obj.method() -> "method").
func scalaScanCalls(code, path string, lineNo int) []Reference {
	var out []Reference
	for _, m := range scalaCallRe.FindAllStringSubmatch(code, -1) {
		name := trailingName(m[1])
		if scalaKeyword[name] {
			continue
		}
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}
