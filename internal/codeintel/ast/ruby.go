// Pure-Go Ruby backend (Phase 13, Tier-2). Ruby is NOT brace-delimited: a method/class/
// module body runs from its `def`/`class`/`module` keyword to the matching `end`. So,
// unlike the brace family, this backend tracks block depth by Ruby's block openers and
// closers rather than `{`/`}` — but it mirrors the Python backend's line-scanner
// STRUCTURE (a stack of open blocks, spans extended as we descend, blocks closed as we
// ascend). It is a deliberately lightweight scanner over the standard library
// (bufio/regexp/strings) — NOT a full grammar.
//
// Goal: reliably surface the structure a code-intel index needs for *typical* Ruby —
// `class`/`module` as types, `def name` as a method (with the enclosing class/module as
// the receiver) or a top-level function, singleton methods `def self.name` (a class
// method, still on the enclosing type), their line spans, and the names they call — not
// to validate or fully model the language.
//
// Block model: every line that OPENS a block (`def`/`class`/`module`/`do`, or a leading
// `if`/`unless`/`while`/`until`/`case`/`begin` that is NOT a one-line statement modifier)
// increments depth; an `end` decrements it. Only `def`/`class`/`module` open a *named*
// block we record as a symbol; the rest (`do`, `if`, ...) are anonymous depth so we can
// find a symbol's matching `end`. A block opened and closed on the same line (`def x; end`
// or a one-liner `... end`) nets to zero and emits a single-line symbol.
//
// Honest scope (heuristic): this models the static `def`/`class`/`module` skeleton only.
// Metaprogramming — `define_method`, `class_eval`, `attr_accessor`, methods conjured by
// `method_missing`, DSLs that generate methods — is invisible here; that dynamic surface
// is the LSP seam's lens, not this static scanner's. Calls are counted only in their
// unambiguous forms — `name(...)` and `recv.method` (with or without args) — because a
// bare `name` with no parens and no receiver is indistinguishable from a local-variable
// reference, so counting it would manufacture false edges. `=begin`/`=end` block comments
// and `#` line comments are stripped; heredocs (`<<~SQL`) are approximated.
package ast

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	// `class NAME` / `module NAME` — a type. NAME may be namespaced (`Foo::Bar`); we keep
	// the trailing segment. A `class << self` singleton-class opener is handled separately
	// (it opens an anonymous block, not a named type).
	rubyClassRe = regexp.MustCompile(`^\s*(?:class|module)\s+([A-Z][A-Za-z0-9_:]*)`)
	rubySelfRe  = regexp.MustCompile(`^\s*class\s*<<\s*\w`)
	// `def name` / `def self.name` / `def Klass.name` — a method. Group 1 (optional) is the
	// singleton qualifier (`self` or a constant) before the `.`; group 2 is the method
	// name. Ruby method names may end in `?`, `!`, or `=`.
	rubyDefRe = regexp.MustCompile(`^\s*def\s+(?:([A-Za-z_][A-Za-z0-9_]*)\.)?([A-Za-z_][A-Za-z0-9_]*[?!=]?)`)

	// Call sites in their unambiguous forms only:
	//   - a (possibly receiver-qualified) name immediately followed by "(":  foo(  obj.bar(
	//   - a receiver selection without parens:  obj.method  (the `.method` form)
	// A bare `name` with no parens and no receiver is intentionally NOT matched (it is
	// ambiguous with a local variable), mirroring the other backends' false-positive
	// avoidance.
	rubyCallParenRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\(`)
	rubyCallDotRe   = regexp.MustCompile(`\.([A-Za-z_][A-Za-z0-9_]*[?!]?)`)

	// Leading block-opening keywords that need a matching `end` (when used as a real
	// compound statement, not a trailing one-line modifier — see rubyOpensBlock).
	rubyOpenerRe = regexp.MustCompile(`^\s*(if|unless|while|until|case|begin|for)\b`)

	// `do` opening a block (`each do |x|` or a trailing `do`). Counts as one open.
	rubyDoRe = regexp.MustCompile(`\bdo\b(\s*\|[^|]*\|)?\s*$`)

	rubyKeyword = map[string]bool{
		"if": true, "unless": true, "while": true, "until": true, "case": true,
		"when": true, "for": true, "begin": true, "rescue": true, "ensure": true,
		"return": true, "yield": true, "super": true, "raise": true, "loop": true,
		"def": true, "class": true, "module": true, "do": true, "end": true,
		"and": true, "or": true, "not": true, "then": true, "elsif": true,
		"else": true, "in": true, "puts": true, "print": true, "require": true,
		"require_relative": true, "attr_accessor": true, "attr_reader": true,
		"attr_writer": true, "include": true, "extend": true, "new": true,
	}
)

// rubyParser scans Ruby source line-by-line. It is stateless across calls.
type rubyParser struct{}

var _ languageParser = rubyParser{}

// rubyBlock tracks an open def/class/module (a NAMED block) so we can nest (a def inside
// a class is a method) and compute spans. We store the symbol's INDEX into the result
// slice (not a pointer — append may reallocate; same rationale as pyBlock). openDepth is
// the block depth right after this header opened; the block closes when depth returns to
// that level. isType marks class/module so nested defs read it as their receiver.
type rubyBlock struct {
	idx       int
	openDepth int
	isType    bool
}

func (rubyParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := rubyScan(path, false)
	return syms, err
}

func (rubyParser) references(path string) ([]Reference, error) {
	_, refs, err := rubyScan(path, true)
	return refs, err
}

func (rubyParser) calls(path string) (map[string][]string, error) {
	syms, refs, err := rubyScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// rubyScan is the shared single pass. It tracks net block depth over Ruby's openers/
// closers and maintains a stack of named (def/class/module) blocks, closing them as `end`
// brings depth back to their open level. Spans of all open named blocks are extended to
// each line before close-out, so a block always covers its body.
func rubyScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := os.Open(path) //nolint:gosec // path is supplied by the worktree-confined walker
	if err != nil {
		return nil, nil, fmt.Errorf("open ruby file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	var syms []Symbol
	var refs []Reference
	var stack []rubyBlock
	depth := 0
	inBlockComment := false

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()

		// `=begin`/`=end` block comments (must start in column 0). Lines inside are inert.
		if inBlockComment {
			if strings.HasPrefix(raw, "=end") {
				inBlockComment = false
			}
			continue
		}
		if strings.HasPrefix(raw, "=begin") {
			inBlockComment = true
			continue
		}

		code := stripRubyComment(raw)
		if strings.TrimSpace(code) == "" {
			continue
		}

		depthBefore := depth

		// Extend every open named block's span to this line.
		for i := range stack {
			if lineNo > syms[stack[i].idx].Span.EndLine {
				syms[stack[i].idx].Span.EndLine = lineNo
			}
		}

		// Header detection happens against depthBefore (the level this block opens at).
		switch {
		case rubySelfRe.MatchString(code):
			// `class << self` — a singleton-class block. Anonymous (no symbol); it only
			// contributes depth so its `end` is accounted for. Defs inside still read the
			// enclosing class as their receiver via nearestRubyType.

		case matchHeader(rubyClassRe, code) != "":
			name := rubyLastSegment(matchHeader(rubyClassRe, code))
			syms = append(syms, Symbol{Name: name, Kind: KindType, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			stack = append(stack, rubyBlock{idx: len(syms) - 1, openDepth: depthBefore, isType: true})

		case rubyDefMatch(code) != nil:
			fn := rubyDefMatch(code)
			kind, recv := KindFunc, ""
			if t := nearestRubyType(syms, stack); t != "" {
				kind, recv = KindMethod, t
			}
			syms = append(syms, Symbol{Name: fn.name, Kind: kind, Recv: recv, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			// A def opens its own block unless it is closed on the same line (`def x; end`).
			if rubyLineDelta(code) > 0 {
				stack = append(stack, rubyBlock{idx: len(syms) - 1, openDepth: depthBefore})
			}
			// Header default-value / argument calls AFTER the def name's first `(`.
			if wantRefs {
				if op := strings.IndexByte(code, '('); op >= 0 {
					refs = append(refs, rubyScanCalls(code[op+1:], path, lineNo)...)
				}
			}

		default:
			if wantRefs {
				refs = append(refs, rubyScanCalls(code, path, lineNo)...)
			}
		}

		depth += rubyLineDelta(code)
		// Close any named blocks whose matching `end` we have now reached.
		for len(stack) > 0 && depth <= stack[len(stack)-1].openDepth {
			stack = stack[:len(stack)-1]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan ruby file: %w", err)
	}
	return syms, refs, nil
}

// rubyDef is a recognized `def` header: the method name and, for a singleton method
// (`def self.name` / `def Klass.name`), a non-empty qualifier marking it as a class
// method. We keep the name; the receiver of the emitted symbol is always the enclosing
// class/module (a `self.` method is still a method on that type), so the qualifier only
// confirms it is a def (not used to override the receiver).
type rubyDef struct {
	name string
}

// rubyDefMatch parses a `def` header, or returns nil. Both `def name` and
// `def self.name` / `def Klass.name` are recognized; the method name is group 2.
func rubyDefMatch(code string) *rubyDef {
	m := rubyDefRe.FindStringSubmatch(code)
	if m == nil {
		return nil
	}
	return &rubyDef{name: m[2]}
}

// rubyLineDelta returns the net block-depth change contributed by one comment/string-
// stripped line: openers (`def`/`class`/`module`/`do`, and a leading compound `if`/
// `unless`/`while`/`until`/`case`/`begin`/`for`) add 1 each; every `end` token subtracts
// 1. A trailing one-line modifier (`x += 1 if cond`) does NOT open a block — only a
// LEADING `if`/etc. does — so rubyOpensBlock gates those.
func rubyLineDelta(code string) int {
	delta := 0
	trimmed := strings.TrimSpace(code)

	// Named openers: a leading def/class/module.
	if strings.HasPrefix(trimmed, "def ") || trimmed == "def" ||
		strings.HasPrefix(trimmed, "class ") || strings.HasPrefix(trimmed, "class<") ||
		strings.HasPrefix(trimmed, "module ") {
		delta++
	}

	// A LEADING compound keyword (if/unless/while/until/case/begin/for) opens a real
	// block; a trailing one-line modifier (`x += 1 if cond`) has the keyword mid-line, so
	// the leading-anchor regex won't match it. A leading `if`/etc. that also closes with
	// `end` on the same line is netted back out by rubyCountEnds below.
	if rubyOpenerRe.MatchString(code) {
		delta++
	}

	// A `do ... |args|` block opener at end of line.
	if rubyDoRe.MatchString(code) {
		delta++
	}

	// Every standalone `end` closes a block. Count word-bounded `end` tokens.
	delta -= rubyCountEnds(code)

	return delta
}

// rubyCountEnds counts word-bounded `end` keyword tokens on a stripped line.
func rubyCountEnds(code string) int {
	n := 0
	for _, f := range tokenize(code) {
		if f == "end" {
			n++
		}
	}
	return n
}

// tokenize splits a line into identifier-ish tokens on any non-[A-Za-z0-9_] boundary, so
// keyword matching is word-bounded (a method named `extend` or a string fragment won't be
// miscounted as the `end` keyword — strings are already blanked by stripRubyComment).
func tokenize(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	})
}

// nearestRubyType returns the name of the nearest enclosing class/module block, or "" if
// none — the receiver type for a def. A top-level def (no enclosing type) is a function.
func nearestRubyType(syms []Symbol, stack []rubyBlock) string {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].isType {
			return syms[stack[i].idx].Name
		}
	}
	return ""
}

// rubyLastSegment returns the final `::`-separated segment of a constant path
// (`Foo::Bar` -> `Bar`).
func rubyLastSegment(name string) string {
	if i := strings.LastIndex(name, "::"); i >= 0 {
		return name[i+2:]
	}
	return name
}

// stripRubyComment blanks string literals and a trailing `#` comment (length-preserving),
// so neither quoted text nor comment text reads as code. It does not span multi-line
// strings — an accepted approximation (heredocs are noted in the package doc).
func stripRubyComment(s string) string {
	b := []rune(s)
	out := make([]rune, len(b))
	inStr := false
	var quote rune
	prevBackslash := false
	for i, r := range b {
		switch {
		case inStr:
			out[i] = ' '
			if r == quote && !prevBackslash {
				inStr = false
			}
		case r == '\'' || r == '"':
			inStr = true
			quote = r
			out[i] = ' '
		case r == '#':
			for j := i; j < len(b); j++ {
				out[j] = ' '
			}
			return string(out)
		default:
			out[i] = r
		}
		prevBackslash = r == '\\' && !prevBackslash
	}
	return string(out)
}

// rubyScanCalls extracts call references from one stripped line, in the two unambiguous
// forms: `name(...)`/`recv.method(...)` (paren calls) and `.method` (receiver selection
// without parens). It drops keyword matches and keeps the trailing simple name for dotted
// calls (`obj.method()` -> "method"). A bare `name` (no parens, no receiver) is not
// counted, to avoid local-variable false positives.
func rubyScanCalls(code, path string, lineNo int) []Reference {
	var out []Reference
	seen := map[int]bool{} // dedupe a `.method(` that both regexes match at the same column
	for _, m := range rubyCallParenRe.FindAllStringSubmatchIndex(code, -1) {
		full := code[m[2]:m[3]]
		name := trailingName(full)
		if rubyKeyword[name] {
			continue
		}
		seen[m[3]] = true
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	for _, m := range rubyCallDotRe.FindAllStringSubmatchIndex(code, -1) {
		if seen[m[3]] {
			continue // already recorded as a paren call (`.method(`)
		}
		name := code[m[2]:m[3]]
		if rubyKeyword[name] {
			continue
		}
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}
