package ast

import (
	"testing"
)

// A representative Java fixture exercising the shapes the backend models: a class with a
// generic+annotated method, a constructor, a method with a `throws` clause that calls a
// helper, a nested (inner) class with its own method, an interface, an enum, a record,
// and an annotation type (`@interface`). Decoys in a line comment, a block comment, and
// a string must NOT register as calls; control keywords (`if`, `for`, `new`) must not
// read as calls either.
const javaSample = `package com.example;

import java.util.List;

// a comment with decoy() that must not register
public class Box<T> {
    private final T value;

    public Box(T value) {
        this.value = value;
    }

    @Override
    public <R> R map(java.util.function.Function<T, R> f) throws Exception {
        /* block decoy() ignored */
        String s = "string decoy() ignored";
        return f.apply(value);
    }

    static class Inner {
        void ping() {
            if (true) {
                helper(new Object());
            }
        }
    }
}

interface Shape {
    double area();
}

enum Color {
    RED, GREEN, BLUE;
}

record Point(int x, int y) {}

@interface Marker {
}
`

func TestJavaSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "Sample.java", javaSample))
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
		recv string
	}{
		{"map", KindMethod, "Box"},
		{"Inner", KindType, ""},
		{"ping", KindMethod, "Inner"},
		{"Shape", KindType, ""},
		{"Color", KindType, ""},
		{"Point", KindType, ""},
		{"Marker", KindType, ""},
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
	}

	// `Box` is both the class type and the constructor name; assert both distinct symbols
	// exist (a name-keyed map would hide one).
	var sawClass, sawCtor bool
	for _, s := range syms {
		if s.Name == "Box" && s.Kind == KindType && s.Recv == "" {
			sawClass = true
		}
		if s.Name == "Box" && s.Kind == KindMethod && s.Recv == "Box" {
			sawCtor = true
		}
	}
	if !sawClass {
		t.Errorf("class type Box not captured: %+v", syms)
	}
	if !sawCtor {
		t.Errorf("constructor Box(...) not captured as a method with recv Box: %+v", syms)
	}
}

func TestJavaSpans(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "Sample.java", javaSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	// map spans its header through its close brace; assert it covers more than one line.
	if s := got["map"]; s.Span.EndLine <= s.Span.StartLine {
		t.Errorf("map span = %d-%d, want a multi-line body", s.Span.StartLine, s.Span.EndLine)
	}
	// Box (outer class TYPE, not its same-named constructor) spans through its inner
	// class's body — well past its header.
	var boxType Symbol
	for _, s := range syms {
		if s.Name == "Box" && s.Kind == KindType {
			boxType = s
		}
	}
	if boxType.Span.EndLine < boxType.Span.StartLine+10 {
		t.Errorf("Box type span = %d-%d, want it to cover the whole class body", boxType.Span.StartLine, boxType.Span.EndLine)
	}
}

func TestJavaReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "Sample.java", javaSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	// f.apply(...) records "apply"; helper(...) is a bare call.
	for _, want := range []string{"apply", "helper"} {
		if !names[want] {
			t.Errorf("expected a reference to %q; got %+v", want, refs)
		}
	}
	// Decoys, the methods' own names, and control keywords must not register.
	for _, bad := range []string{"decoy", "map", "ping", "if", "new"} {
		if names[bad] {
			t.Errorf("decoy/self-call/keyword %q leaked into references: %+v", bad, refs)
		}
	}
}

func TestJavaCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "Sample.java", javaSample))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(calls["map"], "apply") {
		t.Errorf("map should call apply (f.apply); got %+v", calls["map"])
	}
	if !contains(calls["ping"], "helper") {
		t.Errorf("ping should call helper; got %+v", calls["ping"])
	}
	for _, fn := range []string{"map", "ping"} {
		if _, ok := calls[fn]; !ok {
			t.Errorf("%s should be a key; got keys %v", fn, keys(calls))
		}
	}
}
