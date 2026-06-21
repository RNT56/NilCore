// Pure-Go Lua backend (Phase 13, Tier-3). Lua is NOT brace-delimited: a function/block body
// runs from its keyword to the matching `end`. So, like the Ruby backend, this tracks block
// depth by Lua's block openers (`function`/`do`/`if`/`for`/`while`) and the `end` closer
// rather than `{`/`}` — but it mirrors the Ruby/Python line-scanner STRUCTURE (a stack of
// open blocks, spans extended as we descend, blocks closed as we ascend). It is a
// deliberately lightweight scanner over the standard library (bufio/regexp/strings) — NOT a
// full grammar.
//
// Function shapes recognized:
//   - `function name(...)`              -> a free function `name`
//   - `local function name(...)`        -> a free function `name`
//   - `function Tbl.method(...)`        -> a method `method` on table `Tbl` (Recv = Tbl)
//   - `function Tbl:method(...)`        -> a method `method` on table `Tbl` (the `:` colon
//     form, with an implicit `self`; Recv = Tbl)
//   - `name = function(...)`            -> a function `name` (anonymous-function assignment)
//   - `Tbl.name = function(...)`        -> a method `name` on `Tbl`
//
// Block model: `function`/`do`/`if`/`for`/`while` open a block that a matching `end` closes;
// `repeat ... until` is closed by `until` (handled). Only the function forms emit a symbol;
// the bare `do`/`if`/`for`/`while` openers are anonymous depth so we can find a function's
// matching `end`. A one-line function (`function f() return 1 end`) nets to zero and emits a
// single-line symbol.
//
// Honest scope (heuristic): this models the static `function` skeleton only. Metaprogramming
// — `setmetatable`, functions stored in tables built at runtime, `load`/`loadstring` — is
// invisible here; that dynamic surface is the LSP seam's lens. Calls are counted in their
// unambiguous forms — `name(...)`, `obj.method(...)`, `obj:method(...)` — keeping the
// trailing simple name. `--` line comments and `--[[ ]]` block comments are stripped; long
// strings (`[[ ]]`) are approximated.
package ast

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	// `function name(` / `local function name(` / `function Tbl.method(` /
	// `function Tbl:method(`. Group 1 (optional) is the receiver table before a `.` or `:`;
	// group 2 is the function/method name.
	luaFuncRe = regexp.MustCompile(`^\s*(?:local\s+)?function\s+(?:([A-Za-z_][A-Za-z0-9_.]*)[.:])?([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

	// `name = function(` / `Tbl.name = function(` — an anonymous function bound to a name.
	// Group 1 (optional) is the receiver table; group 2 is the bound name.
	luaAssignFuncRe = regexp.MustCompile(`^\s*(?:local\s+)?(?:([A-Za-z_][A-Za-z0-9_.]*)\.)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*function\s*\(`)

	// Call sites: `name(`, `obj.method(`, `obj:method(`. The colon-call form (`obj:m(`) is a
	// method call; we keep the trailing simple name.
	luaCallRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*(?:[.:][A-Za-z_][A-Za-z0-9_]*)*)\s*\(`)

	luaKeyword = map[string]bool{
		"if": true, "for": true, "while": true, "repeat": true, "until": true,
		"return": true, "function": true, "do": true, "else": true, "elseif": true,
		"then": true, "end": true, "local": true, "and": true, "or": true,
		"not": true, "in": true, "break": true, "goto": true, "nil": true,
		"true": true, "false": true,
	}
)

// luaParser scans Lua source line-by-line. It is stateless across calls.
type luaParser struct{}

var _ languageParser = luaParser{}

// luaBlock tracks an open named function block so we can nest and compute spans. We store the
// symbol's INDEX (not a pointer — append may reallocate; same rationale as pyBlock).
// openDepth is the block depth right after this header opened; the block closes when depth
// returns to that level.
type luaBlock struct {
	idx       int
	openDepth int
}

func (luaParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := luaScan(path, false)
	return syms, err
}

func (luaParser) references(path string) ([]Reference, error) {
	_, refs, err := luaScan(path, true)
	return refs, err
}

func (luaParser) calls(path string) (map[string][]string, error) {
	syms, refs, err := luaScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// luaScan is the shared single pass. It tracks net block depth over Lua's openers/closers and
// maintains a stack of named function blocks, closing them as `end` brings depth back to their
// open level. Spans of all open blocks are extended to each line before close-out.
func luaScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := os.Open(path) //nolint:gosec // path is supplied by the worktree-confined walker
	if err != nil {
		return nil, nil, fmt.Errorf("open lua file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	var syms []Symbol
	var refs []Reference
	var stack []luaBlock
	depth := 0
	inBlockComment := false

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()

		// `--[[ ... ]]` block comments. Lines inside are inert until the closing `]]`.
		if inBlockComment {
			if strings.Contains(raw, "]]") {
				inBlockComment = false
			}
			continue
		}

		code := stripLuaComment(raw)
		if strings.TrimSpace(code) == "" {
			// A block comment may have opened on a line whose code part is now empty.
			if strings.Contains(raw, "--[[") && !strings.Contains(raw, "]]") {
				inBlockComment = true
			}
			continue
		}
		if strings.Contains(raw, "--[[") && !strings.Contains(raw, "]]") {
			inBlockComment = true
		}

		depthBefore := depth

		// Extend every open block's span to this line.
		for i := range stack {
			if lineNo > syms[stack[i].idx].Span.EndLine {
				syms[stack[i].idx].Span.EndLine = lineNo
			}
		}

		if fn := luaFuncMatch(code); fn != nil {
			syms = append(syms, Symbol{Name: fn.name, Kind: fn.kind, Recv: fn.recv, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			// A function opens its own block unless it is closed on the same line.
			if luaLineDelta(code) > 0 {
				stack = append(stack, luaBlock{idx: len(syms) - 1, openDepth: depthBefore})
			}
			// Header argument / default calls after the function name's first `(`.
			if wantRefs {
				if op := strings.IndexByte(code, '('); op >= 0 {
					refs = append(refs, luaScanCalls(code[op+1:], path, lineNo)...)
				}
			}
		} else if wantRefs {
			refs = append(refs, luaScanCalls(code, path, lineNo)...)
		}

		depth += luaLineDelta(code)
		// Close any blocks whose matching `end` we have now reached.
		for len(stack) > 0 && depth <= stack[len(stack)-1].openDepth {
			stack = stack[:len(stack)-1]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan lua file: %w", err)
	}
	return syms, refs, nil
}

// luaFuncDecl is a recognized function header: its name, the receiver table (for the
// `Tbl.method`/`Tbl:method`/`Tbl.name = function` forms; "" for a free function), and the
// kind it maps to (method when it has a receiver, else function).
type luaFuncDecl struct {
	name string
	recv string
	kind Kind
}

// luaFuncMatch parses a function header in any recognized form, or returns nil. The
// `function`-keyword forms are tried first, then the `name = function(...)` assignment form.
// A dotted receiver path keeps its last segment (`a.b.method` -> Recv `b`).
func luaFuncMatch(code string) *luaFuncDecl {
	if m := luaFuncRe.FindStringSubmatch(code); m != nil {
		return luaDecl(m[1], m[2])
	}
	if m := luaAssignFuncRe.FindStringSubmatch(code); m != nil {
		return luaDecl(m[1], m[2])
	}
	return nil
}

// luaDecl builds a luaFuncDecl from a (possibly empty, possibly dotted) receiver and a name.
func luaDecl(recv, name string) *luaFuncDecl {
	kind := KindFunc
	if recv != "" {
		kind = KindMethod
		if i := strings.LastIndex(recv, "."); i >= 0 {
			recv = recv[i+1:]
		}
	}
	return &luaFuncDecl{name: name, recv: recv, kind: kind}
}

// luaLineDelta returns the net block-depth change for one comment/string-stripped line.
// Each of `function`/`if`/`while`/`for`/`repeat` opens exactly one block; `end` (and
// `until`, which closes a `repeat`) closes one. The subtlety: `for`/`while` headers end in
// `do` and `if`/`elseif` headers contain `then`, but those `do`/`then` tokens are PART of
// the same opener, not new blocks — so we count the leading compound keyword and ignore a
// `do`/`then` that belongs to it. A BARE `do ... end` block (a `do` not consumed by a
// preceding `for`/`while`) opens its own block. We count word-bounded tokens (via tokenize)
// so a name like `endpoint` or `do_thing` is never miscounted as a keyword.
func luaLineDelta(code string) int {
	delta := 0
	toks := tokenize(code)
	// pendingDo tracks whether the next `do` token is the tail of a `for`/`while` header
	// (and thus already accounted for by that opener), so only a free-standing `do` adds depth.
	pendingDo := false
	for _, tok := range toks {
		switch tok {
		case "function", "if", "repeat":
			delta++
		case "for", "while":
			delta++
			pendingDo = true // the `do` that closes this header is not a new block
		case "do":
			if pendingDo {
				pendingDo = false // consumed by the for/while opener
			} else {
				delta++ // a bare `do ... end` block
			}
		case "end", "until":
			delta--
		}
	}
	return delta
}

// stripLuaComment blanks string literals and a trailing `--` line comment (length-preserving),
// so neither quoted text nor comment text reads as code. It does not span multi-line long
// strings — an accepted approximation per the package note.
func stripLuaComment(s string) string {
	b := []rune(s)
	out := make([]rune, len(b))
	inStr := false
	var quote rune
	prevBackslash := false
	for i := 0; i < len(b); i++ {
		r := b[i]
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
		case r == '-' && i+1 < len(b) && b[i+1] == '-':
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

// luaScanCalls extracts call references from one stripped line in the unambiguous `name(`,
// `obj.method(`, and `obj:method(` forms, dropping keyword matches and keeping the trailing
// simple name for dotted/colon calls (obj:method() -> "method").
func luaScanCalls(code, path string, lineNo int) []Reference {
	var out []Reference
	for _, m := range luaCallRe.FindAllStringSubmatch(code, -1) {
		name := luaTrailingName(m[1])
		if luaKeyword[name] {
			continue
		}
		out = append(out, Reference{Name: name, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}

// luaTrailingName returns the last `.`/`:`-separated segment of a call target, so
// obj.method -> "method", obj:method -> "method", and foo -> "foo".
func luaTrailingName(full string) string {
	idx := -1
	for i := len(full) - 1; i >= 0; i-- {
		if full[i] == '.' || full[i] == ':' {
			idx = i
			break
		}
	}
	if idx >= 0 {
		return full[idx+1:]
	}
	return full
}
