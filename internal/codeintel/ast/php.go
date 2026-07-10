// Pure-Go PHP backend (Phase 13, Tier-2). Like the other brace-family backends, this
// is a deliberately lightweight, brace-aware line scanner over the standard library
// (bufio/regexp/strings) — NOT a full grammar. The goal is to reliably surface the
// structure a code-intel index needs for *typical* PHP — free functions and methods
// (`function name(...)`), classes/interfaces/traits/enums as types, methods carrying
// the enclosing class as their receiver, namespaces, the line spans of those bodies,
// and the names they call — not to validate or fully model the language.
//
// Span model: PHP bodies are brace-delimited (see brace.go). A class/interface/trait/
// enum opens a receiver block so the `function`s inside become methods on that type;
// a free `function` at file scope (or inside a namespace block) is a function. A
// `namespace Foo;` (statement form, no braces) records a type but opens no block; the
// braced form `namespace Foo { ... }` opens a non-receiver span block.
//
// PHP-specific stripping: source lives inside `<?php ... ?>` tags interleaved with HTML.
// We blank the tags themselves (so `<?php` never reads as anything) and, in addition to
// the `//` / `/* */` / string handling brace.go already does, blank `#` line comments
// (PHP's third comment form). HTML outside the tags is left as-is; it rarely contains
// the `function`/`class` lexemes and never the brace shapes we key on, so it is inert.
//
// Honest scope (heuristic, like rust.go): visibility/`static`/`abstract`/`final`
// modifiers, return-type declarations (`: int`), and union/nullable types are tolerated,
// but full resolution — traits' `use`d methods, magic `__call`, variable functions
// `$fn()`, the dynamic `$obj->$name()` form — is the LSP seam's job. The HTML/PHP
// interleaving and heredoc/nowdoc (`<<<EOT`) bodies are approximated.
package ast

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"
)

var (
	phpMods = `(?:public\s+|private\s+|protected\s+|static\s+|abstract\s+|final\s+|readonly\s+)*`

	// `namespace Foo` or `namespace Foo\Bar` — statement (`;`) or braced (`{`) form.
	// Captures the (possibly `\`-separated) name; we keep the trailing segment.
	phpNamespaceRe = regexp.MustCompile(`^\s*namespace\s+([A-Za-z_][A-Za-z0-9_\\]*)`)

	// A type declaration: `class`/`interface`/`trait`/`enum NAME`. The name stops before
	// an `extends`/`implements` clause, a backing-type `:` (enums), or the opening brace.
	phpTypeRe = regexp.MustCompile(`^\s*` + phpMods + `(?:class|interface|trait|enum)\s+([A-Za-z_][A-Za-z0-9_]*)`)

	// A function or method header: `function NAME(`. Modifiers and an optional `&`
	// (return-by-reference) may precede the name. Captures the identifier before `(`.
	phpFuncRe = regexp.MustCompile(`^\s*` + phpMods + `function\s+&?\s*([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

	// Call sites. PHP selects with `->` (instance) and `::` (static); free calls are bare.
	// We capture the dotted/selected target and keep the trailing simple name. The
	// optional leading `$` on the receiver (`$obj->m`, but the method name itself has no
	// `$`) is consumed by blanking `$` before the regex runs (see phpScanCalls).
	phpCallRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*(?:(?:->|::)[A-Za-z_][A-Za-z0-9_]*)*)\s*\(`)

	// Keywords that take a "(" but are control flow / language constructs, not calls.
	phpKeyword = map[string]bool{
		"if": true, "elseif": true, "for": true, "foreach": true, "while": true,
		"switch": true, "catch": true, "function": true, "return": true, "echo": true,
		"print": true, "isset": true, "unset": true, "empty": true, "list": true,
		"array": true, "new": true, "throw": true, "match": true, "declare": true,
		"do": true, "else": true, "case": true, "and": true, "or": true, "xor": true,
		"instanceof": true, "clone": true, "yield": true, "require": true,
		"require_once": true, "include": true, "include_once": true, "exit": true,
		"die": true,
	}
)

// phpParser scans PHP source line-by-line. It is stateless across calls.
type phpParser struct{}

var _ languageParser = phpParser{}

func (phpParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := phpScan(path, false)
	return syms, err
}

func (phpParser) references(path string) ([]Reference, error) {
	_, refs, err := phpScan(path, true)
	return refs, err
}

func (phpParser) calls(path string) (map[string][]string, error) {
	syms, refs, err := phpScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// phpScan is the shared single pass; see jsScan for the structure. Header precedence: a
// namespace/type opens a (receiver, for types) block; a `function` inside a type is a
// method, otherwise a free function. The `#` comment form and the PHP tags are blanked
// before brace.go's stripLine handles `//` / `/* */` / strings.
func phpScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := openSource(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open php file: %w", err)
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
		raw := phpBlankTags(sc.Text())
		code, nextSt := stripLine(raw, st)
		code = phpBlankHash(code)
		depthBefore := depth
		delta := braceDelta(code)

		extendBraceSpans(syms, stack, lineNo)

		switch {
		case matchHeader(phpNamespaceRe, code) != "":
			name := phpLastSegment(matchHeader(phpNamespaceRe, code))
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			// Braced namespace opens a span block (not a receiver — its members are free
			// functions); the statement form (`;`, no `{`) opens nothing.
			if strings.Contains(code, "{") {
				stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore})
			}

		case matchHeader(phpTypeRe, code) != "":
			name := matchHeader(phpTypeRe, code)
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore, isRecv: true})

		case matchHeader(phpFuncRe, code) != "":
			kind, recv := KindFunc, ""
			if r := nearestRecv(syms, stack); r != "" {
				kind, recv = KindMethod, r
			}
			pushBraceFunc(&syms, &stack, path, lineNo, matchHeader(phpFuncRe, code), recv, kind, depthBefore)
			scanHeaderCalls(&refs, code, path, lineNo, wantRefs, phpScanCalls)

		default:
			if wantRefs {
				refs = append(refs, phpScanCalls(code, path, lineNo)...)
			}
		}

		depth += delta
		closeBraceBlocks(&stack, depth)
		st = nextSt
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan php file: %w", err)
	}
	return syms, refs, nil
}

// phpLastSegment returns the final `\`-separated segment of a namespace name
// (`App\Models` -> `Models`).
func phpLastSegment(name string) string {
	if i := strings.LastIndex(name, "\\"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// phpBlankTags blanks the `<?php`, `<?=`, `<?`, and `?>` markers (length-preserving) so
// they never read as code. The PHP keywords/braces inside them are untouched.
func phpBlankTags(line string) string {
	for _, tag := range []string{"<?php", "<?=", "<?", "?>"} {
		for {
			i := strings.Index(line, tag)
			if i < 0 {
				break
			}
			line = line[:i] + strings.Repeat(" ", len(tag)) + line[i+len(tag):]
		}
	}
	return line
}

// phpBlankHash blanks a `#` line comment (PHP's third comment form, which brace.go's
// stripLine does not handle) on an already string/`//`-stripped line. Because strings
// are already blanked by stripLine, any `#` we see is a real comment start (PHP's `#[`
// attribute syntax also begins with `#`, so a trailing `#[Attr]` line is blanked too —
// harmless, attributes carry no symbols we key on). Length is preserved.
func phpBlankHash(code string) string {
	if i := strings.IndexByte(code, '#'); i >= 0 {
		return code[:i] + strings.Repeat(" ", len(code)-i)
	}
	return code
}

// phpScanCalls extracts call references from one stripped line. It first blanks `$`
// sigils so `$obj->method(` exposes `obj->method(` to the regex, then normalizes `->`
// and `::` selectors to `.` so trailingName yields the final segment, dropping keyword
// matches and keeping the trailing simple name (`$obj->method()` -> "method",
// `Class::make()` -> "make").
func phpScanCalls(code, path string, lineNo int) []Reference {
	code = strings.ReplaceAll(code, "$", " ")
	var out []Reference
	for _, m := range phpCallRe.FindAllStringSubmatch(code, -1) {
		full := strings.ReplaceAll(strings.ReplaceAll(m[1], "->", "."), "::", ".")
		name := trailingName(full)
		if phpKeyword[name] {
			continue
		}
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}
