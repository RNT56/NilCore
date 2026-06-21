package ast

import (
	"testing"
)

// A representative C fixture: a typedef, a struct and an enum (tag declarations), a
// pointer-returning function, a static function whose body calls helpers and uses
// control keywords + sizeof + return( that must NOT read as calls, and a prototype
// (declaration with no body) that must NOT be captured as a definition. Decoys in a line
// comment, a block comment, and a string must not register.
const cSample = `#include <stdlib.h>

typedef int Handle;

struct Point {
    int x;
    int y;
};

enum Color { RED, GREEN, BLUE };

// a comment with decoy() that must not register
int add(int a, int b);

char *concat(const char *a, const char *b) {
    /* block decoy() ignored */
    const char *s = "string decoy() ignored";
    return strcat(dup(a), b);
}

static int sum(int n) {
    int total = 0;
    for (int i = 0; i < n; i++) {
        total += scale(i, sizeof(int));
    }
    return (total);
}
`

func TestCSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.c", cSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}

	tests := []struct {
		name string
		kind Kind
	}{
		{"Handle", KindType},
		{"Point", KindType},
		{"Color", KindType},
		{"concat", KindFunc},
		{"sum", KindFunc},
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
		// C has no receivers.
		if s.Recv != "" {
			t.Errorf("%s: recv = %q, want empty (C has no receivers)", tc.name, s.Recv)
		}
	}

	// `int add(int a, int b);` is a prototype, not a definition — must NOT be a symbol.
	if _, ok := got["add"]; ok {
		t.Errorf("prototype add(...) must not be captured as a function definition: %+v", syms)
	}
}

func TestCSpans(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.c", cSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	// concat's body is multi-line.
	if s := got["concat"]; s.Span.EndLine <= s.Span.StartLine {
		t.Errorf("concat span = %d-%d, want a multi-line body", s.Span.StartLine, s.Span.EndLine)
	}
}

func TestCReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "sample.c", cSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	for _, want := range []string{"strcat", "dup", "scale"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	// Decoys, the function's own name, control keywords, sizeof, and `return (` must not
	// register.
	for _, bad := range []string{"decoy", "concat", "sum", "for", "if", "sizeof", "return"} {
		if names[bad] {
			t.Errorf("decoy/self-call/keyword %q leaked into references: %+v", bad, refs)
		}
	}
}

func TestCCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "sample.c", cSample))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(calls["concat"], "strcat") || !contains(calls["concat"], "dup") {
		t.Errorf("concat should call strcat and dup; got %+v", calls["concat"])
	}
	if !contains(calls["sum"], "scale") {
		t.Errorf("sum should call scale; got %+v", calls["sum"])
	}
	for _, fn := range []string{"concat", "sum"} {
		if _, ok := calls[fn]; !ok {
			t.Errorf("%s should be a key; got keys %v", fn, keys(calls))
		}
	}
}

// A .h header dispatches to the C backend and extracts the same shapes.
func TestCHeaderExtension(t *testing.T) {
	src := "struct Node { int v; };\nint init(void) { return setup(); }\n"
	syms, err := Symbols(writeSrc(t, "api.h", src))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	if s, ok := got["Node"]; !ok || s.Kind != KindType {
		t.Errorf("Node struct not extracted from .h: %+v", syms)
	}
	if s, ok := got["init"]; !ok || s.Kind != KindFunc {
		t.Errorf("init function not extracted from .h: %+v", syms)
	}
}
