// Pure-Go Bash backend (Phase 13, Tier-3). Like the brace-family backends, this is a
// deliberately lightweight, brace-aware line scanner over the standard library
// (bufio/regexp/strings) — NOT a full grammar. The goal is to reliably surface the
// structure a code-intel index needs for *typical* shell scripts — function definitions and
// a self-contained call graph between them — not to validate or fully model the language.
//
// Function shapes: shell functions appear as `name() {` (POSIX form) or `function name {` /
// `function name() {` (ksh/bash form). The body is `{ ... }`, so we reuse the shared brace
// machinery (brace.go) to span it by net brace depth. Each function is emitted as a
// KindFunc symbol — shell has no methods/receivers, so Recv is always "".
//
// Call resolution (heuristic — this is the important caveat): a function is invoked simply
// by writing its name as a bare command at the start of a statement, with NO call syntax
// distinguishing it from any external program (`grep`, `ls`) or builtin (`echo`, `cd`).
// Counting every bare command as a "call" would flood the graph with noise from external
// tools. So this backend counts ONLY invocations of names that are ALSO defined as functions
// in the SAME file — a self-contained, intra-file call graph. A call to a function defined
// in another sourced file is therefore invisible here (an accepted miss); a call to an
// external program is correctly ignored. Resolution is two-pass: collect the file's function
// names first, then keep only references whose name is in that set.
//
// Stripping: `#` line comments, single/double-quoted strings, and heredoc bodies (`<<EOF` /
// `<<-EOF` / `<<'EOF'`) are blanked (approximately) so quoted/commented text and heredoc
// content never read as a function call or a brace. Heredoc handling is line-based: from the
// `<<TAG` line until a line equal to the (whitespace-trimmed) TAG, the body is inert.
package ast

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"
)

var (
	// `name() {` (POSIX) or `function name` / `function name()` (ksh/bash). Group 1 (POSIX)
	// or group 2 (function-keyword) is the function name. A shell function name may contain
	// `-`, `.`, `:`, and `+` in addition to word characters.
	bashPosixFnRe = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_.:+-]*)\s*\(\s*\)`)
	bashKwFnRe    = regexp.MustCompile(`^\s*function\s+([A-Za-z_][A-Za-z0-9_.:+-]*)`)

	// A bare command at the start of a statement: the first token on a line, or the first
	// token after a `;`, `&&`, `||`, `|`, `&`, `(`, or `{` command separator. We capture the
	// command word and later keep only those that name a defined function. Leading
	// whitespace and the separator are consumed by the non-capturing prefix.
	bashCommandRe = regexp.MustCompile(`(?:^|[;&|({]|&&|\|\|)\s*([A-Za-z_][A-Za-z0-9_.:+-]*)`)

	// A heredoc opener: `<<TAG`, `<<-TAG`, `<<'TAG'`, `<<"TAG"`. Group 1 is the (unquoted)
	// tag. We approximate by blanking lines until a line whose trimmed text equals the tag.
	bashHeredocRe = regexp.MustCompile(`<<-?\s*["']?([A-Za-z_][A-Za-z0-9_]*)["']?`)
)

// bashParser scans shell source line-by-line. It is stateless across calls.
type bashParser struct{}

var _ languageParser = bashParser{}

func (bashParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := bashScan(path, false)
	return syms, err
}

func (bashParser) references(path string) ([]Reference, error) {
	_, refs, err := bashScan(path, true)
	return refs, err
}

func (bashParser) calls(path string) (map[string][]string, error) {
	syms, refs, err := bashScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// bashScan is the shared single pass. It tracks brace depth to span function bodies and
// collects candidate command references; after the walk it keeps only the references whose
// name resolves to a function defined in this file (the self-contained call-graph heuristic).
func bashScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := openSource(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open bash file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	var syms []Symbol
	var candidates []Reference // every bare-command reference; filtered to defined funcs below
	var stack []braceBlock
	var st stripState
	depth := 0
	heredocTag := "" // non-empty while inside a heredoc body, holding the terminator tag

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()

		// Inside a heredoc body: the line is inert until the terminator tag appears alone.
		if heredocTag != "" {
			if strings.TrimSpace(raw) == heredocTag {
				heredocTag = ""
			}
			extendBraceSpans(syms, stack, lineNo)
			continue
		}

		code, nextSt := stripLine(raw, st)
		depthBefore := depth
		delta := braceDelta(code)

		extendBraceSpans(syms, stack, lineNo)

		if name := bashFnName(code); name != "" {
			// A function definition. The `{` may be on this line (common) or the next; if the
			// body brace opens here, span it via a brace block, else emit a single-line symbol
			// and rely on the next `{`-bearing line to nest. We open a block when the line's
			// net delta is positive (a `{` opened).
			syms = append(syms, Symbol{Name: name, Kind: KindFunc, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			if delta > 0 {
				stack = append(stack, braceBlock{idx: len(syms) - 1, openDepth: depthBefore})
			}
		} else if wantRefs {
			candidates = append(candidates, bashScanCommands(code, path, lineNo)...)
		}

		depth += delta
		closeBraceBlocks(&stack, depth)
		st = nextSt

		// A heredoc opener on this line starts a body on the following lines.
		if m := bashHeredocRe.FindStringSubmatch(code); m != nil {
			heredocTag = m[1]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan bash file: %w", err)
	}

	// Resolve candidate references against the defined-function set: only intra-file function
	// invocations survive, giving a self-contained call graph free of external-command noise.
	defined := map[string]bool{}
	for _, s := range syms {
		defined[s.Name] = true
	}
	var refs []Reference
	for _, r := range candidates {
		if defined[r.Name] {
			refs = append(refs, r)
		}
	}
	return syms, refs, nil
}

// bashFnName returns the name declared by a function-definition line, or "" if the line is
// not a definition. Both `name() {` and `function name` forms are recognized.
func bashFnName(code string) string {
	if m := bashKwFnRe.FindStringSubmatch(code); m != nil {
		return m[1]
	}
	if m := bashPosixFnRe.FindStringSubmatch(code); m != nil {
		return m[1]
	}
	return ""
}

// bashScanCommands extracts every bare-command reference from one stripped line — the first
// word of each statement (after a command separator). These are CANDIDATES; the caller keeps
// only the ones that name a function defined in the same file.
func bashScanCommands(code, path string, lineNo int) []Reference {
	var out []Reference
	for _, m := range bashCommandRe.FindAllStringSubmatch(code, -1) {
		out = append(out, Reference{Name: m[1], Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}
