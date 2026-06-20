// Shared helpers for the brace-delimited heuristic backends (JavaScript/TypeScript
// and Rust). Neither language has a pure-Go standard-library parser we can reach,
// and a cgo tree-sitter binding is forbidden by the zero-cgo invariant (I6) — so both
// backends are deliberately lightweight line scanners over the standard library
// (bufio/regexp/strings). They are NOT full grammars.
//
// Two concerns are common to both languages and live here so the per-language files
// stay focused on their own header/keyword shapes:
//
//   - String/comment stripping. Before scanning a line for call sites we must hide
//     anything inside string literals or comments, so a "(" or an identifier living
//     in quotes or after // never reads as a call. JS template literals (`...`) and
//     Rust both use the same `'`, `"`, `//`, `/* */` lexemes, so one stripper serves
//     both (Rust char literals like 'a' are treated as short strings — harmless for
//     call detection). Multi-line block comments and multi-line template/strings are
//     tracked across lines via a small carry state.
//
//   - Brace-depth span tracking. Both languages delimit a function/type body with
//     `{ ... }`, so a header's span runs from its line to the line of the matching
//     close brace. We track net brace depth (over comment/string-stripped text) and
//     close the innermost open header when depth returns to the header's open level.
package ast

import (
	"regexp"
	"strings"
)

// stripState carries cross-line lexer context for stripLine: whether we are inside a
// block comment (/* ... */) or a multi-line string/template literal, and which quote
// opened it. Callers thread one value through the whole file.
type stripState struct {
	inBlockComment bool
	inString       bool
	quote          byte // the opening quote of an open string: ' " or `
}

// stripLine returns the line with all string-literal and comment content replaced by
// spaces, plus the updated carry state. Lengths are preserved (every removed byte
// becomes a space) so any column math stays stable; we only consume line numbers, but
// stable lengths keep the call regex offsets sane. We operate on bytes: identifiers,
// quotes, and the comment/brace lexemes we care about are all ASCII, and any
// multi-byte UTF-8 sequence only appears inside strings/comments/identifiers where its
// individual bytes are >= 0x80 and so never collide with those ASCII delimiters.
func stripLine(line string, st stripState) (string, stripState) {
	b := []byte(line)
	out := make([]byte, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case st.inBlockComment:
			// Look for the closing */; blank everything until then.
			out[i] = ' '
			if c == '*' && i+1 < len(b) && b[i+1] == '/' {
				out[i+1] = ' '
				i++
				st.inBlockComment = false
			}
		case st.inString:
			out[i] = ' '
			if c == '\\' && st.quote != '`' {
				// Escape: blank the next byte too so an escaped quote doesn't close
				// the string. Template literals don't treat \ specially for our needs,
				// but standard quotes do.
				if i+1 < len(b) {
					out[i+1] = ' '
					i++
				}
				continue
			}
			if c == st.quote {
				st.inString = false
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			// Line comment: blank the rest of the line.
			for ; i < len(b); i++ {
				out[i] = ' '
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			out[i] = ' '
			out[i+1] = ' '
			i++
			st.inBlockComment = true
		case c == '\'' || c == '"' || c == '`':
			st.inString = true
			st.quote = c
			out[i] = ' '
		default:
			out[i] = c
		}
	}
	return string(out), st
}

// braceDelta returns the net change in brace depth contributed by one already-stripped
// line: (count of '{') - (count of '}'). Because the line is comment/string-stripped,
// braces inside literals or comments are already gone.
func braceDelta(stripped string) int {
	return strings.Count(stripped, "{") - strings.Count(stripped, "}")
}

// braceBlock tracks an open brace-delimited header (function/type/impl) while we walk
// a file, so we can compute its span by depth. We store the symbol's INDEX into the
// result slice rather than a pointer: the slice grows via append as symbols are
// discovered, which can relocate the backing array and dangle a held pointer — an
// index stays valid across reallocation (same rationale as the Python backend's
// pyBlock). openDepth is the brace depth *after* the header line's own braces were
// counted; the block closes when depth returns to that level.
// A block may or may not correspond to an emitted symbol: a JS function/class and a
// Rust fn/struct each emit one (idx points at it, so span extension updates it), but a
// Rust `impl Type` block emits no symbol of its own — it only supplies the receiver
// name for the methods nested inside it. Such a block carries idx == noSym and holds
// its receiver name in recvName instead.
const noSym = -1

type braceBlock struct {
	idx       int    // index into the syms slice, or noSym for a symbol-less container
	openDepth int    // brace depth right after the header line
	isRecv    bool   // a type container (JS class / Rust impl) whose name receives methods
	recvName  string // receiver name when idx == noSym (e.g. a Rust impl target)
}

// matchHeader returns the first capture group of re against code, or "" if it does not
// match. It exists so callers can test "did this header match, and what is its name?"
// in one expression.
func matchHeader(re *regexp.Regexp, code string) string {
	m := re.FindStringSubmatch(code)
	if m == nil {
		return ""
	}
	return m[1]
}

// trailingName returns the last dotted segment of a (possibly dotted) call target, so
// obj.method -> "method" and foo -> "foo". Rust path calls (a::b::c) are split the same
// way after the caller normalizes "::" to ".".
func trailingName(full string) string {
	if dot := strings.LastIndex(full, "."); dot >= 0 {
		return full[dot+1:]
	}
	return full
}

// pushBraceFunc appends a function/method symbol and opens a brace block for it so its
// span will be extended until the matching close brace. recv is the enclosing receiver
// type name ("" for free functions). depthBefore is the brace depth before this line's
// braces were counted, which becomes the block's close-out level.
func pushBraceFunc(syms *[]Symbol, stack *[]braceBlock, path string, lineNo int, name, recv string, kind Kind, depthBefore int) {
	*syms = append(*syms, Symbol{
		Name: name,
		Kind: kind,
		Recv: recv,
		Span: Span{File: path, StartLine: lineNo, EndLine: lineNo},
	})
	*stack = append(*stack, braceBlock{idx: len(*syms) - 1, openDepth: depthBefore})
}

// nearestRecv returns the name of the nearest enclosing receiver block (JS class /
// Rust impl) on the stack, or "" if none — the receiver type for a method. A
// symbol-less container (Rust impl) supplies its name from recvName; a symbol-backed
// container (JS class) supplies it from the emitted symbol.
func nearestRecv(syms []Symbol, stack []braceBlock) string {
	for i := len(stack) - 1; i >= 0; i-- {
		if !stack[i].isRecv {
			continue
		}
		if stack[i].idx == noSym {
			return stack[i].recvName
		}
		return syms[stack[i].idx].Name
	}
	return ""
}

// extendBraceSpans stretches the EndLine of every still-open block that backs a symbol
// to include lineNo. Called once per line before close-out, so a block's span always
// covers the lines its body actually occupies. Symbol-less containers (idx == noSym)
// have no span to extend.
func extendBraceSpans(syms []Symbol, stack []braceBlock, lineNo int) {
	for i := range stack {
		if stack[i].idx == noSym {
			continue
		}
		if lineNo > syms[stack[i].idx].Span.EndLine {
			syms[stack[i].idx].Span.EndLine = lineNo
		}
	}
}

// closeBraceBlocks pops every open block whose matching close brace we have now passed,
// i.e. whose openDepth the current depth has returned to (or fallen below). Because the
// innermost block always has the highest openDepth, popping from the top in a loop
// closes them in the right order.
func closeBraceBlocks(stack *[]braceBlock, depth int) {
	s := *stack
	for len(s) > 0 && depth <= s[len(s)-1].openDepth {
		s = s[:len(s)-1]
	}
	*stack = s
}

// groupBraceCalls attributes each call reference to the innermost function/method whose
// span contains it, producing the per-function call map for the brace-delimited
// backends. Every function/method is seeded as a key (even with no calls) so callers
// see the full set, mirroring the Go backend keying on every FuncDecl. Calls outside
// any function are dropped — the call graph is function-to-function edges.
func groupBraceCalls(syms []Symbol, refs []Reference) map[string][]string {
	out := map[string][]string{}
	for _, s := range syms {
		if s.Kind == KindFunc || s.Kind == KindMethod {
			if _, ok := out[s.Name]; !ok {
				out[s.Name] = nil
			}
		}
	}
	for _, r := range refs {
		if owner := innermostFunc(syms, r.Span.StartLine); owner != "" {
			out[owner] = append(out[owner], r.Name)
		}
	}
	return out
}
