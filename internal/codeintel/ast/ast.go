// Package ast is the structural foundation of code intelligence (P3-T09): parse
// source to symbols (functions, types, methods, vars, consts) and references (the
// "tag map"), each carrying a source span. Parsing is per-file, so re-parsing a
// single changed file is inherently incremental.
//
// Scope note: this is a multi-language seam (D3-T01). The public API — Symbols /
// References / Calls plus the Symbol/Reference/Span/Kind types — is the stable
// contract; behind it sits a per-language backend keyed by file extension. Go is
// served by the standard library (go/parser); Python, JS/TS, Rust, and the Tier-1
// brace-delimited languages (Java, C, C++, C#) by pure-Go heuristic line scanners.
// Everything here is stdlib only — no cgo, no tree-sitter — so the zero-cgo build
// holds. A richer per-language backend (a tree-sitter binding behind an external
// service, say) can slot in later without changing callers.
package ast

import (
	"path/filepath"
	"strings"
)

// Kind classifies a symbol.
type Kind string

const (
	KindFunc   Kind = "func"
	KindMethod Kind = "method"
	KindType   Kind = "type"
	KindVar    Kind = "var"
	KindConst  Kind = "const"
)

// Span is a source location (1-based line range).
type Span struct {
	File      string
	StartLine int
	EndLine   int
}

// Symbol is a declared name.
type Symbol struct {
	Name string
	Kind Kind
	Recv string // receiver type, for methods
	Span Span
}

// Reference is a use of a name (call or selection).
type Reference struct {
	Name string
	Span Span
}

// languageParser is the per-language backend behind the public API. Each backend
// owns one source language and answers the same three questions about a file. The
// seam exists so a new language (Python here) is additive: register a backend,
// callers are untouched. Backends read the file themselves (they differ on how —
// go/parser takes a path+FileSet, the Python scanner streams lines) so the seam
// stays at the path, not at a shared token stream.
type languageParser interface {
	symbols(path string) ([]Symbol, error)
	references(path string) ([]Reference, error)
	calls(path string) (map[string][]string, error)
}

// parsers maps a (lower-cased) file extension, dot included, to its backend. It is
// the single source of truth for both dispatch and SupportedExtensions, so the two
// can never drift.
var parsers = map[string]languageParser{
	".go": goParser{},
	".py": pythonParser{},
	// JavaScript/TypeScript share one brace-delimited heuristic backend; the .ts/.tsx
	// extensions ride the same scanner since TS is a JS superset for our purposes.
	".js":  jsParser{},
	".jsx": jsParser{},
	".ts":  jsParser{},
	".tsx": jsParser{},
	".mjs": jsParser{},
	".cjs": jsParser{},
	".rs":  rustParser{},
	// Tier-1 brace-delimited languages (Phase 13). Each has a dedicated heuristic
	// line-scanner backend reusing the shared brace machinery (brace.go).
	".java": javaParser{},
	// C: .c sources and .h headers (the .h family is shared with C++ below, but C is
	// the conservative default for a bare .h — only C++-specific files claim .hpp/.hh/
	// .hxx).
	".c": cParser{},
	".h": cParser{},
	// C++: source and header variants.
	".cc":  cppParser{},
	".cpp": cppParser{},
	".cxx": cppParser{},
	".hpp": cppParser{},
	".hh":  cppParser{},
	".hxx": cppParser{},
	// C#.
	".cs": csharpParser{},
}

// SupportedExtensions returns the file extensions (dot included, e.g. ".go", ".py")
// that this package can parse. The index walk uses it to decide which files to feed
// in: callers should treat any other extension as "skip" — Symbols/References/Calls
// return (nil, nil) for them rather than erroring, so a broadened walk degrades
// gracefully on mixed-language trees. The result is freshly allocated and unsorted;
// callers that need a stable order should sort it.
func SupportedExtensions() []string {
	exts := make([]string, 0, len(parsers))
	for ext := range parsers {
		exts = append(exts, ext)
	}
	return exts
}

// parserFor resolves the backend for a path by its extension, or nil if the
// extension is unsupported. The lookup is case-insensitive so ".PY" still dispatches
// (extensions are conventionally lower-case, but we don't want a stray upper-case
// suffix to silently drop a file).
func parserFor(path string) languageParser {
	return parsers[strings.ToLower(filepath.Ext(path))]
}

// Symbols extracts the declared symbols from a source file. The language is chosen
// by file extension; an unsupported extension returns (nil, nil) so walkers skip it
// cleanly without treating it as an error.
func Symbols(path string) ([]Symbol, error) {
	p := parserFor(path)
	if p == nil {
		return nil, nil
	}
	return p.symbols(path)
}

// References extracts called/selected names (the reference edges) from a source
// file. Unsupported extensions return (nil, nil); see Symbols.
func References(path string) ([]Reference, error) {
	p := parserFor(path)
	if p == nil {
		return nil, nil
	}
	return p.references(path)
}

// Calls returns, per top-level function/method, the names it calls — the raw
// material for the call graph (P3-T10). Unsupported extensions return (nil, nil);
// see Symbols.
func Calls(path string) (map[string][]string, error) {
	p := parserFor(path)
	if p == nil {
		return nil, nil
	}
	return p.calls(path)
}
