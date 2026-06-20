package ast

import (
	"testing"
)

// A representative Rust fixture: a free fn, a struct, an enum, a trait, an impl block
// with two methods (one calling a free fn, one a path call), bare/path/selector call
// sites, and decoys inside a line comment, a block comment, and a string literal that
// must NOT register as calls. A struct literal (`Point { ... }`) must not read as a
// call either (no parens).
const rsSample = `use std::fmt;

// a comment with decoy() that must not register
pub fn add(a: i32, b: i32) -> i32 {
    a + b
}

struct Point {
    x: i32,
    y: i32,
}

enum Shape {
    Circle,
    Square,
}

trait Area {
    fn area(&self) -> i32;
}

impl Point {
    pub fn new(x: i32, y: i32) -> Point {
        /* block decoy() ignored */
        let _ = "string decoy() ignored";
        Point { x, y }
    }

    fn norm(&self) -> i32 {
        add(self.x, helpers::scale(self.y))
    }
}
`

func TestRustSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.rs", rsSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}

	tests := []struct {
		name  string
		kind  Kind
		recv  string
		start int
	}{
		{"add", KindFunc, "", 4},
		{"Point", KindType, "", 8},
		{"Shape", KindType, "", 13},
		{"Area", KindType, "", 18},
		{"new", KindMethod, "Point", 23},
		{"norm", KindMethod, "Point", 29},
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
		if s.Span.StartLine != tc.start {
			t.Errorf("%s: start line = %d, want %d", tc.name, s.Span.StartLine, tc.start)
		}
	}

	// The `impl Point` block must NOT emit a second Point symbol — only the struct
	// declares the type.
	var pointCount int
	for _, s := range syms {
		if s.Name == "Point" && s.Kind == KindType {
			pointCount++
		}
	}
	if pointCount != 1 {
		t.Errorf("Point declared %d times, want 1 (impl block must not re-declare it)", pointCount)
	}
}

func TestRustSpans(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.rs", rsSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	// add spans its header (4) to its close brace (6).
	if s := got["add"]; s.Span.StartLine != 4 || s.Span.EndLine != 6 {
		t.Errorf("add span = %d-%d, want 4-6", s.Span.StartLine, s.Span.EndLine)
	}
	// new is a method body running 23-27.
	if s := got["new"]; s.Span.StartLine != 23 || s.Span.EndLine != 27 {
		t.Errorf("new span = %d-%d, want 23-27", s.Span.StartLine, s.Span.EndLine)
	}
}

func TestRustReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "sample.rs", rsSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	// add() is a bare call; helpers::scale() records the trailing path segment "scale".
	for _, want := range []string{"add", "scale"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	// Decoys and the fn headers' own names must not register.
	for _, bad := range []string{"decoy", "new", "norm", "add_self"} {
		if names[bad] {
			t.Errorf("decoy/self-call %q leaked into references: %+v", bad, refs)
		}
	}
}

func TestRustCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "sample.rs", rsSample))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(calls["norm"], "add") {
		t.Errorf("norm should call add; got %+v", calls["norm"])
	}
	if !contains(calls["norm"], "scale") {
		t.Errorf("norm should call scale (helpers::scale); got %+v", calls["norm"])
	}
	// add() and new() must be keys; new() calls nothing the call graph tracks.
	for _, fn := range []string{"add", "new", "norm"} {
		if _, ok := calls[fn]; !ok {
			t.Errorf("%s should be a key; got keys %v", fn, keys(calls))
		}
	}
}

// A type-generic impl written with NO space after `impl` — `impl<T> Wrapper<T>`,
// `impl<T: Clone> Wrapper<T>`, `impl<T, U> Pair<T, U>` — is idiomatic and must still
// open a receiver block, so its methods are methods of the type, not free functions.
// (Regression: the regex once required `impl\s+`, missing every no-space generic.)
//
// Lifetime-parameterized impls (`impl<'a> Parser<'a>`) are a documented limitation,
// not covered here: the shared line stripper treats a Rust lifetime `'a` as a char
// literal, so the receiver is lost — an accepted heuristic-scanner approximation
// (the LSP seam is the precise lens). Kept explicit so the boundary is intentional.
func TestRustGenericImplReceiver(t *testing.T) {
	for _, src := range []string{
		"struct Wrapper<T> { v: T }\n\nimpl<T> Wrapper<T> {\n    fn get(&self) -> i32 { 0 }\n}\n",
		"struct Wrapper<T> { v: T }\n\nimpl<T: Clone> Wrapper<T> {\n    fn get(&self) -> i32 { 0 }\n}\n",
		"struct Pair<T, U> { a: T, b: U }\n\nimpl<T, U> Pair<T, U> {\n    fn get(&self) -> i32 { 0 }\n}\n",
	} {
		syms, err := Symbols(writeSrc(t, "gen.rs", src))
		if err != nil {
			t.Fatal(err)
		}
		var get Symbol
		var found bool
		for _, s := range syms {
			if s.Name == "get" {
				get, found = s, true
			}
		}
		if !found {
			t.Fatalf("get not extracted from %q", src)
		}
		if get.Kind != KindMethod || (get.Recv != "Wrapper" && get.Recv != "Pair") {
			t.Errorf("generic-impl method get: kind=%q recv=%q, want a method on Wrapper/Pair\nsrc: %s", get.Kind, get.Recv, src)
		}
	}
}

// `impl Trait for Type` must set the receiver to the implemented-for type, not the
// trait — the type after `for` wins.
func TestRustImplTraitForReceiver(t *testing.T) {
	src := "trait Draw {\n    fn draw(&self);\n}\n\nstruct Widget;\n\nimpl Draw for Widget {\n    fn draw(&self) {\n        render(self);\n    }\n}\n"
	syms, err := Symbols(writeSrc(t, "impl.rs", src))
	if err != nil {
		t.Fatal(err)
	}
	var draw Symbol
	for _, s := range syms {
		if s.Name == "draw" && s.Kind == KindMethod {
			draw = s
		}
	}
	if draw.Recv != "Widget" {
		t.Errorf("`impl Draw for Widget` method recv = %q, want Widget", draw.Recv)
	}
}
