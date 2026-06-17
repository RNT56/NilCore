package ast

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// A representative Python fixture: a top-level function, a class with a method, a
// nested function, call sites (bare, dotted, and a constructor), comments and a
// string literal containing decoy parens to confirm they are ignored.
const pySample = `import math


def add(a, b):
    # a comment with parens ( ) that must not register
    return a + b


class Calc:
    """A docstring: not_a_call() should be ignored."""

    def __init__(self, n):
        self.n = n

    def double(self):
        return add(self.n, self.n)


def outer():
    def inner():
        return helper()
    return inner()
`

func writePy(t *testing.T, src string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sample.py")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPythonSymbols(t *testing.T) {
	syms, err := Symbols(writePy(t, pySample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}

	tests := []struct {
		name    string
		kind    Kind
		recv    string
		started int
	}{
		{"add", KindFunc, "", 4},
		{"Calc", KindType, "", 9},
		{"__init__", KindMethod, "Calc", 12},
		{"double", KindMethod, "Calc", 15},
		{"outer", KindFunc, "", 19},
		{"inner", KindFunc, "", 20},
	}
	for _, tc := range tests {
		s, ok := got[tc.name]
		if !ok {
			t.Errorf("%s: not extracted", tc.name)
			continue
		}
		if s.Kind != tc.kind {
			t.Errorf("%s: kind = %q, want %q", tc.name, s.Kind, tc.kind)
		}
		if s.Recv != tc.recv {
			t.Errorf("%s: recv = %q, want %q", tc.name, s.Recv, tc.recv)
		}
		if s.Span.StartLine != tc.started {
			t.Errorf("%s: start line = %d, want %d", tc.name, s.Span.StartLine, tc.started)
		}
	}
}

func TestPythonSymbolSpans(t *testing.T) {
	syms, err := Symbols(writePy(t, pySample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	// `add` is a two-line body (lines 4-6): header through the last indented line.
	if s := got["add"]; s.Span.StartLine != 4 || s.Span.EndLine != 6 {
		t.Errorf("add span = %d-%d, want 4-6", s.Span.StartLine, s.Span.EndLine)
	}
	// `Calc` spans its whole suite, through `double`'s body.
	if s := got["Calc"]; s.Span.StartLine != 9 || s.Span.EndLine < 16 {
		t.Errorf("Calc span = %d-%d, want 9 through at least 16", s.Span.StartLine, s.Span.EndLine)
	}
	// `outer` contains `inner`; its span must reach inner's body and the final return.
	if s := got["outer"]; s.Span.StartLine != 19 || s.Span.EndLine < 22 {
		t.Errorf("outer span = %d-%d, want 19 through at least 22", s.Span.StartLine, s.Span.EndLine)
	}
}

func TestPythonReferences(t *testing.T) {
	refs, err := References(writePy(t, pySample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	for _, want := range []string{"add", "helper", "inner"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	// The decoy call inside the docstring/comment must NOT register.
	if names["not_a_call"] {
		t.Errorf("docstring decoy not_a_call() leaked into references")
	}
	// `double` calls add at line 16.
	var sawAddAt16 bool
	for _, r := range refs {
		if r.Name == "add" && r.Span.StartLine == 16 {
			sawAddAt16 = true
		}
	}
	if !sawAddAt16 {
		t.Errorf("expected reference to add at line 16; got %+v", refs)
	}
}

func TestPythonCalls(t *testing.T) {
	calls, err := Calls(writePy(t, pySample))
	if err != nil {
		t.Fatal(err)
	}
	// double() calls add().
	if !contains(calls["double"], "add") {
		t.Errorf("double should call add; got %+v", calls["double"])
	}
	// inner() calls helper(); outer() calls inner().
	if !contains(calls["inner"], "helper") {
		t.Errorf("inner should call helper; got %+v", calls["inner"])
	}
	if !contains(calls["outer"], "inner") {
		t.Errorf("outer should call inner; got %+v", calls["outer"])
	}
	// add() calls nothing, but must still be a key (mirrors the Go backend).
	if _, ok := calls["add"]; !ok {
		t.Errorf("add should be a key even with no calls; got keys %v", keys(calls))
	}
}

func TestPythonAsyncDef(t *testing.T) {
	src := "async def fetch(url):\n    return await get(url)\n"
	syms, err := Symbols(writePy(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 1 || syms[0].Name != "fetch" || syms[0].Kind != KindFunc {
		t.Errorf("async def not extracted as func: %+v", syms)
	}
}

func TestPythonScanError(t *testing.T) {
	// A missing file must surface an error, not a silent empty result.
	if _, err := Symbols(filepath.Join(t.TempDir(), "nope.py")); err == nil {
		t.Error("expected an error for a missing .py file")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func keys(m map[string][]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
