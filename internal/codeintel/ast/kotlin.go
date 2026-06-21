// Pure-Go Kotlin backend (Phase 13, Tier-2). Like the other brace-family backends, this
// is a deliberately lightweight, brace-aware line scanner over the standard library
// (bufio/regexp/strings) — NOT a full grammar. The goal is to reliably surface the
// structure a code-intel index needs for *typical* Kotlin — top-level and member
// functions (`fun name(...)`), extension functions (`fun Foo.bar(...)`),
// class/interface/object/enum/data/sealed types, members carrying the enclosing type as
// their receiver, the line spans of those bodies, and the names they call — not to
// validate or fully model the language.
//
// Span model: Kotlin bodies are brace-delimited (see brace.go). A class/interface/
// object/`enum class`/`data class`/`sealed class` opens a receiver block so the `fun`s
// inside become methods on that type; a top-level `fun` is a function. A `companion
// object` opens a receiver block whose name is the enclosing type (so its members read
// the type as their Recv), matching how callers reference them as `Type.member`.
//
// Extension receivers: `fun Foo.bar(...)` declares `bar` on `Foo` regardless of nesting
// — the receiver is the qualifier before the `.`, captured from the header itself, not
// the stack. This mirrors how Rust's out-of-line `Type::method` carries its own Recv.
//
// Honest scope (heuristic, like rust.go): annotations (`@Foo`), generics (`fun <T>`),
// `suspend`/`inline`/`operator`/`infix`/visibility modifiers, and `=`-expression bodies
// (`fun f() = expr`) are tolerated, but full resolution — overloads, type aliases,
// delegated properties, the `by` clause — is the LSP seam's job. An `=`-bodied `fun`
// with no `{` is captured as a single-line symbol. Pathological inputs (a `{` on a later
// line, multi-line string templates `"""..."""`, lambdas with receivers) may be
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
	ktMods = `(?:public\s+|private\s+|protected\s+|internal\s+|open\s+|final\s+|abstract\s+|sealed\s+|inner\s+|override\s+|suspend\s+|inline\s+|operator\s+|infix\s+|tailrec\s+|external\s+|const\s+|lateinit\s+|annotation\s+)*`

	// A type declaration: `class`/`interface`/`object`/`enum class`/`data class`/
	// `sealed class`/`annotation class NAME`. The leading `(?:data|enum|sealed|...)\s+`
	// qualifiers are folded into the modifier set and the `class`/`interface`/`object`
	// keyword. The name stops before a generic `<`, a primary-constructor `(`, a
	// supertype `:`, or the opening brace.
	ktTypeRe = regexp.MustCompile(`^\s*` + ktMods + `(?:data\s+|enum\s+|sealed\s+|value\s+)*(?:class|interface|object)\s+([A-Za-z_][A-Za-z0-9_]*)`)

	// `companion object` (optionally named). A receiver container; its members are
	// referenced via the enclosing type, so its Recv is taken from the stack, not its own
	// (possibly absent) name.
	ktCompanionRe = regexp.MustCompile(`^\s*` + ktMods + `companion\s+object\b`)

	// A function header: `fun NAME(` or `fun Recv.NAME(`, with an optional generic
	// `<...>` between `fun` and the name. Group 1 (optional) is the extension receiver
	// (the qualifier before the final `.`, which may itself carry generics like
	// `Box<Int>` or be dotted like `com.x.Foo`); group 2 is the function name.
	ktFuncRe = regexp.MustCompile(`^\s*` + ktMods + `fun\s+(?:<[^>]*>\s*)?(?:([A-Za-z_][A-Za-z0-9_.<>, ]*)\.)?([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

	// Call sites: identifier optionally dotted (obj.method, this.helper) before "(".
	ktCallRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\(`)

	ktKeyword = map[string]bool{
		"if": true, "for": true, "while": true, "when": true, "catch": true,
		"return": true, "throw": true, "is": true, "as": true, "in": true,
		"fun": true, "do": true, "else": true, "init": true, "constructor": true,
		"super": true, "this": true, "by": true, "where": true, "object": true,
	}
)

// kotlinParser scans Kotlin source line-by-line. It is stateless across calls.
type kotlinParser struct{}

var _ languageParser = kotlinParser{}

func (kotlinParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := kotlinScan(path, false)
	return syms, err
}

func (kotlinParser) references(path string) ([]Reference, error) {
	_, refs, err := kotlinScan(path, true)
	return refs, err
}

func (kotlinParser) calls(path string) (map[string][]string, error) {
	syms, refs, err := kotlinScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// kotlinScan is the shared single pass; see jsScan for the structure. Header precedence:
// a type or companion object opens a receiver block; a `fun` is a method when it has an
// explicit extension receiver, or when nested inside a type, else a free function.
func kotlinScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := os.Open(path) //nolint:gosec // path is supplied by the worktree-confined walker
	if err != nil {
		return nil, nil, fmt.Errorf("open kotlin file: %w", err)
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
		case ktCompanionRe.MatchString(code):
			// A companion object's members are referenced through the enclosing type, so
			// the receiver block carries that type's name (from the stack), not the
			// companion's own optional name. A symbol-less block keeps it out of the symbol
			// list (the companion is not itself a callable).
			stack = append(stack, braceBlock{idx: noSym, openDepth: depthBefore, isRecv: true, recvName: nearestRecv(syms, stack)})

		case matchHeader(ktTypeRe, code) != "":
			name := matchHeader(ktTypeRe, code)
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			// Open a receiver block only if the body braces open on this line; a type with
			// no body (e.g. `data class P(val x: Int)`) nests nothing.
			if strings.Contains(code, "{") {
				stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore, isRecv: true})
			}

		case ktFuncMatch(code) != nil:
			fn := ktFuncMatch(code)
			kind, recv := KindFunc, ""
			switch {
			case fn.recv != "":
				// An explicit extension receiver (`fun Foo.bar`) wins regardless of nesting.
				kind, recv = KindMethod, fn.recv
			case nearestRecv(syms, stack) != "":
				kind, recv = KindMethod, nearestRecv(syms, stack)
			}
			// Open a block only when a brace body opens on this line; an `=`-expression
			// body (`fun f() = expr`) has no block and stays a single-line symbol.
			if strings.Contains(code, "{") {
				pushBraceFunc(&syms, &stack, path, lineNo, fn.name, recv, kind, depthBefore)
			} else {
				syms = append(syms, Symbol{Name: fn.name, Kind: kind, Recv: recv, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			}
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, kotlinScanCalls)

		default:
			if wantRefs {
				refs = append(refs, kotlinScanCalls(code, path, lineNo)...)
			}
		}

		depth += delta
		closeBraceBlocks(&stack, depth)
		st = nextSt
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan kotlin file: %w", err)
	}
	return syms, refs, nil
}

// ktFuncDecl is a recognized function header: its name and (for extension functions) the
// receiver type — the qualifier before the final `.`, with any leading package/type path
// collapsed to the last segment.
type ktFuncDecl struct {
	name string
	recv string
}

// ktFuncMatch parses a `fun` header on a line, or returns nil. The captured receiver
// (group 1) may be a dotted path (`fun com.x.Foo.bar`); we keep its last segment as the
// Recv.
func ktFuncMatch(code string) *ktFuncDecl {
	m := ktFuncRe.FindStringSubmatch(code)
	if m == nil {
		return nil
	}
	recv := ""
	if m[1] != "" {
		recv = m[1]
		// Drop a generic argument list on the receiver (`Box<Int>` -> `Box`) before taking
		// the last dotted segment (`com.x.Foo` -> `Foo`).
		if lt := strings.IndexByte(recv, '<'); lt >= 0 {
			recv = recv[:lt]
		}
		if i := strings.LastIndex(recv, "."); i >= 0 {
			recv = recv[i+1:]
		}
	}
	return &ktFuncDecl{name: m[2], recv: recv}
}

// kotlinScanCalls extracts call references from one stripped line, dropping keyword
// matches and keeping the trailing simple name for dotted calls (obj.method() ->
// "method").
func kotlinScanCalls(code, path string, lineNo int) []Reference {
	var out []Reference
	for _, m := range ktCallRe.FindAllStringSubmatch(code, -1) {
		name := trailingName(m[1])
		if ktKeyword[name] {
			continue
		}
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}
