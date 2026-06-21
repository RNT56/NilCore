// Pure-Go SQL backend (Phase 13, Tier-3). SQL is neither brace- nor `def`/`end`-delimited:
// the structural units a code-intel index cares about are the schema objects introduced by
// `CREATE` statements. So this backend is a keyword scanner — it walks the comment/string-
// stripped text and emits one symbol per `CREATE` object. It is a deliberately lightweight
// scanner over the standard library (bufio/regexp/strings) — NOT a full SQL grammar.
//
// Objects recognized (case-insensitive), each from a `CREATE [OR REPLACE] {KIND}
// [IF NOT EXISTS] name`:
//   - TABLE, VIEW, MATERIALIZED VIEW, TRIGGER, INDEX, TYPE, SCHEMA -> KindType
//   - FUNCTION, PROCEDURE                                         -> KindFunc
//
// Name policy: a schema-qualified name is KEPT verbatim (`public.users` stays `public.users`,
// not collapsed to `users`). This preserves the disambiguation two same-named objects in
// different schemas need; callers that want the bare name can split on the last `.`.
// Quoted identifiers (`"My Table"`, backtick “ `tbl` “) have their quotes stripped to the
// inner text.
//
// Span model: a CREATE statement's body runs to its terminating `;`, but tracking that across
// the many dialect-specific terminators (`;`, `$$`, `GO`, `/`) is more fragility than value
// for an index — so each object is emitted as a single-line symbol at its CREATE line. SQL
// has no lexical nesting receivers, so Recv is always "".
//
// References (heuristic): SQL "calls" are ambiguous — a bare identifier may be a table, a
// column, a function, or a keyword, and most appear without call syntax. To avoid flooding
// the graph with false edges, this backend records references ONLY for `name(...)` call
// syntax whose name resolves to a FUNCTION/PROCEDURE defined in the SAME file (a self-
// contained call graph, mirroring the Bash backend). Cross-file calls and built-in functions
// are intentionally not recorded. `--` line comments, `/* */` block comments, and string
// literals are stripped.
package ast

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	// `CREATE [OR REPLACE] [TEMP|TEMPORARY|GLOBAL|...] {KIND} [IF NOT EXISTS] name`. Group 1
	// is the object kind (possibly two words, e.g. `MATERIALIZED VIEW`); group 2 is the
	// object name (a dotted, possibly-quoted identifier path). Case-insensitive (?i).
	sqlCreateRe = regexp.MustCompile(`(?i)\bcreate\s+(?:or\s+replace\s+)?(?:temp(?:orary)?\s+|global\s+|local\s+|unlogged\s+|unique\s+)*(materialized\s+view|table|view|function|procedure|trigger|index|type|schema)\s+(?:if\s+not\s+exists\s+)?` + "([A-Za-z_\"`][A-Za-z0-9_.\"`]*)")

	// Call sites: an identifier (optionally schema-qualified) immediately before "(". Used for
	// the function/procedure call graph; only names resolving to a defined FUNCTION/PROCEDURE
	// survive.
	sqlCallRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_.]*)\s*\(`)
)

// sqlParser scans SQL source. It is stateless across calls.
type sqlParser struct{}

var _ languageParser = sqlParser{}

func (sqlParser) symbols(path string) ([]Symbol, error) {
	syms, _, err := sqlScan(path, false)
	return syms, err
}

func (sqlParser) references(path string) ([]Reference, error) {
	_, refs, err := sqlScan(path, true)
	return refs, err
}

func (sqlParser) calls(path string) (map[string][]string, error) {
	syms, refs, err := sqlScan(path, true)
	if err != nil {
		return nil, err
	}
	return groupBraceCalls(syms, refs), nil
}

// sqlScan is the shared single pass. It strips comments/strings, emits a symbol per CREATE
// object, and collects candidate call references; after the walk it keeps only references
// resolving to a FUNCTION/PROCEDURE defined in this file (the self-contained call graph).
func sqlScan(path string, wantRefs bool) ([]Symbol, []Reference, error) {
	f, err := os.Open(path) //nolint:gosec // path is supplied by the worktree-confined walker
	if err != nil {
		return nil, nil, fmt.Errorf("open sql file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	var syms []Symbol
	var candidates []Reference
	var st stripState
	lastFnIdx := -1 // index of the most recent FUNCTION/PROCEDURE symbol, for span extension

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		// SQL comments are `--`/`/* */` (not C's `//`), so we strip them ourselves; the
		// block-comment carry is threaded across lines via stripState.
		code, nextSt := stripSQLLine(raw, st)
		st = nextSt

		if m := sqlCreateRe.FindStringSubmatch(code); m != nil {
			// A new CREATE statement ends the previous FUNCTION/PROCEDURE body: extend that
			// routine's span to the line before this one, so body calls are attributed to it by
			// the per-function call grouping. (We have no reliable, dialect-independent body
			// terminator — `;`, `$$`, `GO`, `/` all vary — so "until the next CREATE or EOF" is
			// the documented heuristic for body extent.)
			if lastFnIdx >= 0 && lineNo-1 > syms[lastFnIdx].Span.EndLine {
				syms[lastFnIdx].Span.EndLine = lineNo - 1
			}
			kind := KindType
			k := strings.ToLower(strings.Join(strings.Fields(m[1]), " "))
			if k == "function" || k == "procedure" {
				kind = KindFunc
			}
			syms = append(syms, Symbol{Name: sqlCleanName(m[2]), Kind: kind, Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
			if kind == KindFunc {
				lastFnIdx = len(syms) - 1
			} else {
				lastFnIdx = -1 // a non-routine CREATE ends the previous routine's call scope
			}
			// The CREATE header line itself is a declaration, not a call body: skip collecting
			// candidate calls from it so the routine's own name (and its parameter list) is
			// never recorded as a self-call.
			continue
		}

		if wantRefs {
			candidates = append(candidates, sqlScanCalls(code, path, lineNo)...)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan sql file: %w", err)
	}
	// Extend the final routine's body span to EOF.
	if lastFnIdx >= 0 && lineNo > syms[lastFnIdx].Span.EndLine {
		syms[lastFnIdx].Span.EndLine = lineNo
	}

	// Resolve candidate calls against the defined FUNCTION/PROCEDURE set (by bare name, so a
	// call `f(...)` matches a `CREATE FUNCTION schema.f`). Only intra-file function calls
	// survive, keeping the graph free of table/column/builtin noise.
	defined := map[string]bool{}
	for _, s := range syms {
		if s.Kind == KindFunc {
			defined[sqlBareName(s.Name)] = true
		}
	}
	var refs []Reference
	for _, r := range candidates {
		if defined[r.Name] {
			refs = append(refs, r)
		}
	}
	return syms, refs, nil
}

// sqlCleanName strips surrounding quotes/backticks from each `.`-segment of an identifier
// path, leaving the schema qualifier intact (`"public"."My Table"` -> `public.My Table`).
func sqlCleanName(name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = strings.Trim(p, "\"`")
	}
	return strings.Join(parts, ".")
}

// sqlBareName returns the trailing (unqualified) segment of a possibly-schema-qualified name,
// so a call `f(...)` resolves against `CREATE FUNCTION schema.f`.
func sqlBareName(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// sqlScanCalls extracts candidate `name(...)` call references from one stripped line, keeping
// the trailing simple name (`schema.f(` -> "f"). These are filtered to defined functions by
// the caller.
func sqlScanCalls(code, path string, lineNo int) []Reference {
	var out []Reference
	for _, m := range sqlCallRe.FindAllStringSubmatch(code, -1) {
		out = append(out, Reference{Name: sqlBareName(m[1]), Span: Span{File: path, StartLine: lineNo, EndLine: lineNo}})
	}
	return out
}

// stripSQLLine blanks SQL comments (`--` line, `/* */` block) and string literals (single-
// quoted, plus double-quoted/backtick identifiers are NOT blanked — they are identifiers, not
// strings) length-preservingly, carrying block-comment state across lines via stripState. We
// reuse stripState's inBlockComment/inString fields. Length is preserved so regex offsets stay
// sane.
func stripSQLLine(line string, st stripState) (string, stripState) {
	b := []byte(line)
	out := make([]byte, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case st.inBlockComment:
			out[i] = ' '
			if c == '*' && i+1 < len(b) && b[i+1] == '/' {
				out[i+1] = ' '
				i++
				st.inBlockComment = false
			}
		case st.inString:
			out[i] = ' '
			// SQL escapes a quote by doubling it (''); a backslash is not special. A doubled
			// quote stays inside the string (we blank both and remain in-string).
			if c == st.quote {
				if i+1 < len(b) && b[i+1] == st.quote {
					out[i+1] = ' '
					i++
				} else {
					st.inString = false
				}
			}
		case c == '-' && i+1 < len(b) && b[i+1] == '-':
			for ; i < len(b); i++ {
				out[i] = ' '
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			out[i] = ' '
			out[i+1] = ' '
			i++
			st.inBlockComment = true
		case c == '\'':
			st.inString = true
			st.quote = '\''
			out[i] = ' '
		default:
			out[i] = c
		}
	}
	return string(out), st
}
