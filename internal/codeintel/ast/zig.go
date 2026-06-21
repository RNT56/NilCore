// Pure-Go Zig backend (Phase 13, Tier-3). Like the other brace-family backends, this is a
// deliberately lightweight, brace-aware line scanner over the standard library
// (bufio/regexp/strings) — NOT a full grammar. The goal is to reliably surface the
// structure a code-intel index needs for *typical* Zig — `fn name(...)` and `pub fn`
// functions/methods, container types declared via the `const NAME = struct { ... }` idiom
// (and `union`/`enum`/`opaque`), members carrying their container as the receiver, the line
// spans of those bodies, and the names they call — not to validate or fully model the
// language.
//
// Container idiom: Zig has no `class` keyword — a type is a value bound to a `const`:
//
//	const Point = struct {
//	    x: f32,
//	    fn dist(self: Point) f32 { ... }   // a method on Point
//	};
//
// So `const NAME = struct {` (or `union`/`enum`/`opaque`) emits a type symbol named NAME and
// opens a receiver block; `fn`s nested directly inside it read NAME as their Recv. A bare
// top-level `fn` (not inside a container) is a free function.
//
// Span model: Zig bodies are brace-delimited (see brace.go). The container's span runs to
// its matching close brace; a `fn` body the same.
//
// Honest scope (heuristic, like rust.go): `pub`/`export`/`extern`/`inline`/`comptime`
// qualifiers, generics-via-`comptime`-params, error sets (`error{...}`), and the trailing
// `;` after a container literal are tolerated, but full resolution — comptime evaluation,
// anonymous struct types, the difference between a method and a namespaced free fn — is the
// LSP seam's job. An anonymous container assigned to a non-const (or returned inline) is not
// captured as a named type. Pathological inputs (a `{` on a later line) may be approximated.
package ast

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	// The container idiom: `const NAME = struct {` / `= union(...) {` / `= enum {` /
	// `= opaque {`. `pub` may precede `const`. Group 1 is the bound name (the type). The
	// container keyword and any `(...)` tag (e.g. `union(enum)`, `enum(u8)`) precede the
	// opening brace.
	zigContainerRe = regexp.MustCompile(`^\s*(?:pub\s+)?const\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(?:extern\s+|packed\s+)?(?:struct|union|enum|opaque)\b`)

	// A function header: `fn NAME(` / `pub fn NAME(` / `export fn NAME(` / `inline fn NAME(`.
	// Group 1 is the function name. We require `(` so only real fn headers match.
	zigFnRe = regexp.MustCompile(`^\s*(?:pub\s+|export\s+|extern\s*(?:"[^"]*"\s*)?|inline\s+|noinline\s+|comptime\s+)*fn\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

	// Call sites: identifier optionally dotted (obj.method, std.debug.print) before "(".
	zigCallRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\(`)

	zigKeyword = map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "catch": true,
		"return": true, "fn": true, "else": true, "try": true, "defer": true,
		"errdefer": true, "comptime": true, "struct": true, "union": true,
		"enum": true, "opaque": true, "error": true, "and": true, "or": true,
		"orelse": true, "unreachable": true, "test": true, "asm": true,
	}
)

// zigParser scans Zig source line-by-line. It is stateless across calls.
type zigParser struct{}

var _ languageParser = zigParser{}

func (zigParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := zigScan(path, false)
	return syms, err
}

func (zigParser) references(path string) ([]Reference, error) {
	_, refs, err := zigScan(path, true)
	return refs, err
}

func (zigParser) calls(path string) (map[string][]string, error) {
	syms, refs, err := zigScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// zigScan is the shared single pass; see jsScan for the structure. Header precedence: a
// `const NAME = struct/union/enum/opaque {` opens a named receiver block; a `fn` is a method
// when nested inside such a container, else a free function.
func zigScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := os.Open(path) //nolint:gosec // path is supplied by the worktree-confined walker
	if err != nil {
		return nil, nil, fmt.Errorf("open zig file: %w", err)
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
		case matchHeader(zigContainerRe, code) != "":
			name := matchHeader(zigContainerRe, code)
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			// Open a receiver block only if the container braces open on this line (the common
			// case); a `{` on a later line is an accepted approximation (the type stays a
			// single-line symbol and its members won't read it as Recv).
			if strings.Contains(code, "{") {
				stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore, isRecv: true})
			}

		case matchHeader(zigFnRe, code) != "":
			name := matchHeader(zigFnRe, code)
			kind, recv := KindFunc, ""
			if r := nearestRecv(syms, stack); r != "" {
				kind, recv = KindMethod, r
			}
			if strings.Contains(code, "{") {
				pushBraceFunc(&syms, &stack, path, lineNo, name, recv, kind, depthBefore)
			} else {
				syms = append(syms, Symbol{Name: name, Kind: kind, Recv: recv, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			}
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, zigScanCalls)

		default:
			if wantRefs {
				refs = append(refs, zigScanCalls(code, path, lineNo)...)
			}
		}

		depth += delta
		closeBraceBlocks(&stack, depth)
		st = nextSt
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan zig file: %w", err)
	}
	return syms, refs, nil
}

// zigScanCalls extracts call references from one stripped line, dropping keyword matches and
// keeping the trailing simple name for dotted calls (std.debug.print() -> "print").
func zigScanCalls(code, path string, lineNo int) []Reference {
	var out []Reference
	for _, m := range zigCallRe.FindAllStringSubmatch(code, -1) {
		name := trailingName(m[1])
		if zigKeyword[name] {
			continue
		}
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}
